package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/price"
)

// ClaudeEnvAllow lists the variables the claude CLI may need for authentication and provider
// selection, on top of the host's default allowlist.
var ClaudeEnvAllow = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "AWS_PROFILE", "AWS_REGION",
	"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID",
}

// Claude runs `claude -p` headless with stream-json output (RN3).
type Claude struct {
	Bin  string    // executable; "claude" by default
	Host host.Host // where the process runs
	// Poll is how often the output file is read while the agent runs.
	Poll time.Duration
	// Prices overrides the built-in token prices used to estimate the cost of runs that end
	// without a result event.
	Prices map[string]price.Rates
}

// NewClaude returns a claude runner using bin and h.
func NewClaude(bin string, h host.Host) *Claude {
	return &Claude{Bin: bin, Host: h, Poll: 100 * time.Millisecond}
}

// Name implements Runner.
func (c *Claude) Name() string { return "claude" }

// Run implements Runner.
func (c *Claude) Run(ctx context.Context, req Request) (Result, error) {
	if req.LogDir == "" {
		return Result{}, errors.New("claude runner: request has no LogDir")
	}
	if err := os.MkdirAll(req.LogDir, 0o755); err != nil {
		return Result{}, err
	}
	base := filepath.Join(req.LogDir, string(req.Role))
	promptPath, streamPath, stderrPath, settingsPath := base+".prompt.md", base+".stream.jsonl", base+".stderr.log", base+".settings.json"

	if err := os.WriteFile(promptPath, []byte(req.Prompt), 0o644); err != nil {
		return Result{}, err
	}
	settings, err := RoleSettings(req.Tools, req.ProtectedPaths)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(settingsPath, settings, 0o644); err != nil {
		return Result{}, err
	}

	h, err := c.Host.Start(ctx, host.ProcSpec{
		Name:       req.Name,
		Argv:       c.argv(req, settingsPath),
		Dir:        req.Dir,
		Env:        req.Env,
		StdinPath:  promptPath,
		StdoutPath: streamPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		return Result{}, fmt.Errorf("claude runner: %w", err)
	}

	if req.OnStart != nil {
		req.OnStart(Started{PID: h.PID(), Command: filepath.Base(c.argv0()), RawPath: streamPath})
	}

	p := &streamParser{onUsage: req.OnUsage, seen: map[string]budget.Usage{}}
	done := make(chan struct{})
	var tailErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tailErr = tailFile(streamPath, c.poll(), done, p.line)
	}()
	ex, waitErr := h.Wait()
	close(done)
	wg.Wait()

	res := p.result(ctx, ex, readTail(stderrPath, 2000), req.Model, c.Prices)
	res.RawPath = streamPath
	// A host failure (e.g. the herdr pane running the worker was closed) is the reason the
	// attempt failed: record it even when the stream already explains a missing result.
	if waitErr != nil && res.Status != StatusCancelled {
		msg := waitErr.Error()
		if res.Status != StatusOK && res.Error != "" {
			msg += "; " + res.Error
		}
		if res.Status == StatusOK {
			res.Status = StatusError
		}
		res.Error = msg
	}
	if tailErr != nil {
		res.Warnings = append(res.Warnings, "reading agent output: "+tailErr.Error())
	}
	return res, nil
}

func (c *Claude) poll() time.Duration {
	if c.Poll <= 0 {
		return 100 * time.Millisecond
	}
	return c.Poll
}

func (c *Claude) argv0() string {
	if c.Bin == "" {
		return "claude"
	}
	return c.Bin
}

func (c *Claude) argv(req Request, settingsPath string) []string {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	a := []string{bin, "-p",
		"--output-format", "stream-json", "--verbose",
		"--settings", settingsPath,
		// Only the repository's own settings: the operator's user-level plugins, hooks and
		// MCP servers stay out of workers (P2: minimal context).
		"--setting-sources", "project",
		"--strict-mcp-config",
		"--no-session-persistence",
		// Anything not allowed by the role settings is denied, never prompted.
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
	}
	if len(req.Tools) > 0 {
		a = append(a, "--tools", strings.Join(req.Tools, ","))
	}
	if req.Model != "" {
		a = append(a, "--model", req.Model)
	}
	if req.Budget.USD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(req.Budget.USD, 'f', -1, 64))
	}
	if len(req.Schema) > 0 {
		a = append(a, "--json-schema", string(req.Schema))
	}
	return a
}

// RoleSettings returns the claude settings JSON for a role: allow exactly tools, and deny
// git operations that belong to mill plus any access to protected paths (SE1).
func RoleSettings(tools []string, protected []string) ([]byte, error) {
	deny := []string{
		"Bash(git push:*)", "Bash(git commit:*)", "Bash(git reset:*)", "Bash(git checkout:*)",
		"Bash(git switch:*)", "Bash(git rebase:*)", "Bash(git merge:*)", "Bash(git branch:*)",
		"Bash(git worktree:*)", "Bash(git tag:*)", "Bash(git remote:*)", "Bash(git config:*)",
		"Bash(sudo:*)", "Bash(mill:*)",
	}
	for _, p := range protected {
		// Claude permission rules use //path for absolute paths.
		rule := "//" + strings.TrimPrefix(filepath.Clean(p), "/") + "/**"
		deny = append(deny, "Read("+rule+")", "Edit("+rule+")", "Write("+rule+")")
	}
	allow := tools
	if allow == nil {
		allow = []string{}
	}
	return json.MarshalIndent(map[string]any{
		"permissions": map[string]any{"allow": allow, "deny": deny},
	}, "", "  ")
}

// streamParser turns stream-json lines into usage reports and a final result.
type streamParser struct {
	mu      sync.Mutex
	onUsage func(budget.Usage)
	seen    map[string]budget.Usage // usage reported so far per API message id
	live    budget.Usage
	final   *resultEvent
	badJSON int
	model   string // model named by the system/init event

	rejected *rateLimitInfo // latest rate_limit_event with status rejected
	warning  string         // usage warning from the first allowed_warning event
}

type rateLimitInfo struct {
	Status        string  `json:"status"`
	ResetsAt      int64   `json:"resetsAt"`
	RateLimitType string  `json:"rateLimitType"`
	Utilization   float64 `json:"utilization"`
}

func (r rateLimitInfo) resetTime() time.Time {
	if r.ResetsAt <= 0 {
		return time.Time{}
	}
	return time.Unix(r.ResetsAt, 0)
}

const localTimeFormat = "2006-01-02 15:04 MST"

type usageJSON struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
}

func (u usageJSON) usage() budget.Usage {
	return budget.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheWriteTokens: u.CacheCreationTokens, CacheReadTokens: u.CacheReadTokens}
}

type resultEvent struct {
	Subtype           string            `json:"subtype"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	TotalCostUSD      float64           `json:"total_cost_usd"`
	DurationMS        int64             `json:"duration_ms"`
	Usage             usageJSON         `json:"usage"`
	TerminalReason    string            `json:"terminal_reason"`
	Errors            []string          `json:"errors"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	APIErrorStatus    int               `json:"api_error_status"`
}

func (p *streamParser) line(b []byte) {
	var ev struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		Model   string          `json:"model"`
		Message json.RawMessage `json:"message"`
		Info    *rateLimitInfo  `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(b, &ev); err != nil {
		p.mu.Lock()
		p.badJSON++
		p.mu.Unlock()
		return
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && ev.Model != "" {
			p.mu.Lock()
			p.model = ev.Model
			p.mu.Unlock()
		}
	case "rate_limit_event":
		if ev.Info == nil {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		switch ev.Info.Status {
		case "rejected":
			info := *ev.Info
			p.rejected = &info
		case "allowed_warning":
			if p.warning == "" {
				msg := fmt.Sprintf("usage at %.0f%% of the %s limit", ev.Info.Utilization*100, ev.Info.RateLimitType)
				if t := ev.Info.resetTime(); !t.IsZero() {
					msg += ", resets at " + t.Local().Format(localTimeFormat)
				}
				p.warning = msg
			}
		}
	case "assistant":
		// message is decoded only here: other event types may carry a string.
		var msg struct {
			ID    string     `json:"id"`
			Usage *usageJSON `json:"usage"`
		}
		if err := json.Unmarshal(ev.Message, &msg); err != nil || msg.Usage == nil {
			return
		}
		// One API message can arrive as several assistant events (one per content block)
		// carrying the same usage; count only the growth per message id.
		u := msg.Usage.usage()
		p.mu.Lock()
		delta := positive(subtract(u, p.seen[msg.ID]))
		p.seen[msg.ID] = maxUsage(u, p.seen[msg.ID])
		p.live = p.live.Add(delta)
		p.mu.Unlock()
		if p.onUsage != nil && delta != (budget.Usage{}) {
			p.onUsage(delta)
		}
	case "result":
		var r resultEvent
		if err := json.Unmarshal(b, &r); err != nil {
			return
		}
		p.mu.Lock()
		p.final = &r
		p.mu.Unlock()
	}
}

// merged returns the usage so far: the result event's when present (authoritative, with the
// cost), else the live assistant usage. Callers hold p.mu.
func (p *streamParser) merged() budget.Usage {
	if p.final == nil {
		return p.live
	}
	final := p.final.Usage.usage()
	u := maxUsage(final, p.live)
	u.CostUSD = p.final.TotalCostUSD
	return u
}

// UsageFromStream parses a claude stream-json file with the rules of the live parser and
// returns the usage it records: assistant usage deduplicated per message id, replaced by the
// result event's usage and cost when there is one. model is named by the system/init event;
// finished reports whether a result event was seen. The cost is zero when !finished.
func UsageFromStream(path string) (u budget.Usage, model string, finished bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return budget.Usage{}, "", false, fmt.Errorf("read agent stream: %w", err)
	}
	defer f.Close()
	p := &streamParser{seen: map[string]budget.Usage{}}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			p.line(line)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return budget.Usage{}, "", false, fmt.Errorf("read agent stream %s: %w", path, rerr)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.merged(), p.model, p.final != nil, nil
}

// result builds the Result after the process has exited.
//
// Without a result event claude reported no cost, so it is estimated from tokens using the
// init event's model (falling back to reqModel).
func (p *streamParser) result(ctx context.Context, ex host.Exit, stderrTail, reqModel string, prices map[string]price.Rates) Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := Result{Usage: p.live}
	if p.final != nil {
		// The result event's usage is authoritative (live output counts lag behind); report
		// the difference, and the cost, which only arrives here.
		final := p.final.Usage.usage()
		final.CostUSD = p.final.TotalCostUSD
		if delta := positive(subtract(final, p.live)); delta != (budget.Usage{}) && p.onUsage != nil {
			p.onUsage(delta)
		}
		res.Usage = p.merged()
		if p.final.DurationMS > 0 {
			res.Duration = time.Duration(p.final.DurationMS) * time.Millisecond
		}
		if n := len(p.final.PermissionDenials); n > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d tool call(s) denied by permission rules", n))
		}
	}
	if p.final == nil && res.Usage != (budget.Usage{}) {
		model := p.model
		if model == "" {
			model = reqModel
		}
		if usd, ok := price.Estimate(model, res.Usage, prices); ok {
			res.Usage.CostUSD, res.CostEstimated = usd, true
			res.Warnings = append(res.Warnings, "cost estimated from tokens (claude reported none)")
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("cost could not be estimated: no known rates for model %q", model))
		}
	}
	if p.warning != "" {
		res.Warnings = append(res.Warnings, p.warning)
	}
	if p.badJSON > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d unparseable output line(s)", p.badJSON))
	}

	switch {
	// A result event means claude finished: honour it even if ctx was cancelled afterwards
	// (e.g. the final usage correction crossed a limit after the work was done).
	case p.final == nil && ctx.Err() != nil:
		res.Status, res.Error = StatusCancelled, ctx.Err().Error()
	case p.final == nil:
		res.Status = StatusError
		res.Error = fmt.Sprintf("claude exited (code %d) without a result", ex.Code)
		if stderrTail != "" {
			res.Error += ": " + stderrTail
		}
	case strings.Contains(p.final.Subtype, "budget"):
		res.Status = StatusBudgetExceeded
		res.Error = "claude stopped at --max-budget-usd (" + p.final.Subtype + ")"
	case p.final.APIErrorStatus == 429 && p.final.IsError, p.rejected != nil && (p.final.IsError || p.final.Subtype != "success"):
		res.Status = StatusRateLimited
		res.Error = "claude usage limit reached"
		if p.rejected != nil {
			res.LimitType, res.ResetsAt = p.rejected.RateLimitType, p.rejected.resetTime()
			if res.LimitType != "" {
				res.Error += " (" + res.LimitType + ")"
			}
			if !res.ResetsAt.IsZero() {
				res.Error += ", resets at " + res.ResetsAt.Local().Format(localTimeFormat)
			}
		}
	case p.final.IsError || p.final.Subtype != "success":
		res.Status = StatusError
		res.Error = strings.TrimSpace(fmt.Sprintf("claude %s: %s %s", p.final.Subtype,
			strings.Join(p.final.Errors, "; "), p.final.Result))
	default:
		out := p.final.StructuredOutput
		if len(out) == 0 || string(out) == "null" {
			// No schema output: accept the final text if it is JSON.
			if json.Valid([]byte(strings.TrimSpace(p.final.Result))) {
				out = json.RawMessage(strings.TrimSpace(p.final.Result))
			}
		}
		if len(out) == 0 {
			res.Status, res.Error = StatusError, "claude finished without structured output"
		} else {
			res.Status, res.Output = StatusOK, out
		}
	}
	return res
}

func subtract(a, b budget.Usage) budget.Usage {
	return budget.Usage{
		InputTokens:      a.InputTokens - b.InputTokens,
		OutputTokens:     a.OutputTokens - b.OutputTokens,
		CacheWriteTokens: a.CacheWriteTokens - b.CacheWriteTokens,
		CacheReadTokens:  a.CacheReadTokens - b.CacheReadTokens,
		CostUSD:          a.CostUSD - b.CostUSD,
	}
}

func positive(u budget.Usage) budget.Usage {
	clamp := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		return v
	}
	cost := u.CostUSD
	if cost < 0 {
		cost = 0
	}
	return budget.Usage{InputTokens: clamp(u.InputTokens), OutputTokens: clamp(u.OutputTokens),
		CacheWriteTokens: clamp(u.CacheWriteTokens), CacheReadTokens: clamp(u.CacheReadTokens), CostUSD: cost}
}

func maxUsage(a, b budget.Usage) budget.Usage {
	m := func(x, y int64) int64 {
		if x > y {
			return x
		}
		return y
	}
	return budget.Usage{InputTokens: m(a.InputTokens, b.InputTokens), OutputTokens: m(a.OutputTokens, b.OutputTokens),
		CacheWriteTokens: m(a.CacheWriteTokens, b.CacheWriteTokens), CacheReadTokens: m(a.CacheReadTokens, b.CacheReadTokens),
		CostUSD: max(a.CostUSD, b.CostUSD)}
}

// tailFile calls fn for each complete line appended to path until done is closed, then
// reads what is left. The file may not exist yet when tailing starts.
func tailFile(path string, poll time.Duration, done <-chan struct{}, fn func([]byte)) error {
	var f *os.File
	var r *bufio.Reader
	var partial []byte
	read := func() error {
		if f == nil {
			var err error
			if f, err = os.Open(path); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			r = bufio.NewReaderSize(f, 1<<20)
		}
		for {
			chunk, err := r.ReadBytes('\n')
			partial = append(partial, chunk...)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			line := partial
			partial = nil
			if len(line) > 1 {
				fn(line[:len(line)-1])
			}
		}
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-done:
			if err := read(); err != nil {
				return err
			}
			if len(strings.TrimSpace(string(partial))) > 0 {
				fn(partial) // final line without a newline
			}
			return nil
		case <-t.C:
			if err := read(); err != nil {
				return err
			}
		}
	}
}

func readTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}
