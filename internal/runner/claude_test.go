package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/price"
)

// Lines shaped like real `claude -p --output-format stream-json --verbose` output.
const (
	lineInit  = `{"type":"system","subtype":"init","tools":["Read"],"model":"claude-haiku"}`
	lineAsst1 = `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":2,"cache_creation_input_tokens":5000,"cache_read_input_tokens":0,"output_tokens":10},"content":[{"type":"text"}]}}`
	// Same API message, second content block: same usage, must not be counted twice.
	lineAsst1b = `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":2,"cache_creation_input_tokens":5000,"cache_read_input_tokens":0,"output_tokens":10},"content":[{"type":"tool_use"}]}}`
	lineAsst2  = `{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":4,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":20},"content":[{"type":"tool_use"}]}}`
	lineResult = `{"type":"result","subtype":"success","is_error":false,"result":"{\"word\":\"hello\"}","structured_output":{"word":"hello"},"total_cost_usd":0.0125,"usage":{"input_tokens":6,"cache_creation_input_tokens":5100,"cache_read_input_tokens":5000,"output_tokens":90},"permission_denials":[]}`
)

// fakeClaude writes an executable that records its argv and stdin, then prints lines.
func fakeClaude(t *testing.T, lines []string, tail string) (bin, argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdinFile = filepath.Join(dir, "stdin")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`for a in "$@"; do printf '%s\n' "$a"; done > ` + argsFile + "\n")
	b.WriteString("cat > " + stdinFile + "\n")
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + strings.ReplaceAll(l, "'", `'\''`) + "'\n")
	}
	b.WriteString(tail + "\n")
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile, stdinFile
}

func newClaude(bin string) *Claude {
	c := NewClaude(bin, host.NewExec())
	c.Poll = 10 * time.Millisecond
	return c
}

func baseReq(t *testing.T) Request {
	return Request{
		Role:   RoleImplementer,
		Prompt: "do the thing",
		Dir:    t.TempDir(),
		LogDir: t.TempDir(),
		Env:    host.WorkerEnv(),
	}
}

func TestClaudeSuccess(t *testing.T) {
	t.Parallel()
	bin, argsFile, stdinFile := fakeClaude(t, []string{lineInit, lineAsst1, lineAsst1b, lineAsst2, lineResult}, "")
	req := baseReq(t)
	req.Model = "sonnet"
	req.Budget = budget.Limits{USD: 1.5, Tokens: 1000}
	req.Schema = json.RawMessage(`{"type":"object"}`)
	req.Tools = []string{"Read", "Edit"}
	req.ProtectedPaths = []string{"/repo/.mill"}

	var mu sync.Mutex
	var deltas []budget.Usage
	req.OnUsage = func(d budget.Usage) { mu.Lock(); deltas = append(deltas, d); mu.Unlock() }

	res, err := newClaude(bin).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || string(res.Output) != `{"word":"hello"}` {
		t.Fatalf("result = %+v", res)
	}
	want := budget.Usage{InputTokens: 6, OutputTokens: 90, CacheWriteTokens: 5100, CacheReadTokens: 5000, CostUSD: 0.0125}
	if res.Usage != want {
		t.Fatalf("usage = %+v, want %+v", res.Usage, want)
	}
	// Live increments (m1 once, m2) plus the final correction add up to the final usage.
	var sum budget.Usage
	for _, d := range deltas {
		sum = sum.Add(d)
	}
	if sum != want {
		t.Fatalf("sum of OnUsage deltas = %+v, want %+v (deltas %+v)", sum, want, deltas)
	}
	if len(deltas) != 3 {
		t.Fatalf("got %d deltas, want 3 (m1, m2, final correction): %+v", len(deltas), deltas)
	}

	args, _ := os.ReadFile(argsFile)
	for _, wantArg := range []string{"-p", "stream-json", "--verbose", "--setting-sources\nproject", "--strict-mcp-config",
		"--no-session-persistence", "--permission-mode\ndontAsk", "--permission-prompts\nnone", "--tools\nRead,Edit",
		"--model\nsonnet", "--max-budget-usd\n1.5", `--json-schema` + "\n" + `{"type":"object"}`} {
		if !strings.Contains(string(args), wantArg) {
			t.Errorf("argv missing %q:\n%s", wantArg, args)
		}
	}
	if stdin, _ := os.ReadFile(stdinFile); string(stdin) != "do the thing" {
		t.Fatalf("prompt not sent on stdin: %q", stdin)
	}
	settings, _ := os.ReadFile(filepath.Join(req.LogDir, "implementer.settings.json"))
	for _, s := range []string{`"Read"`, `"Edit"`, "Bash(git push:*)", "Read(//repo/.mill/**)", "Edit(//repo/.mill/**)"} {
		if !strings.Contains(string(settings), s) {
			t.Errorf("settings missing %s:\n%s", s, settings)
		}
	}
	if res.RawPath == "" {
		t.Fatal("RawPath not set")
	}
	if raw, _ := os.ReadFile(res.RawPath); !strings.Contains(string(raw), `"type":"result"`) {
		t.Fatal("raw stream not kept")
	}
}

func TestClaudeNoBudgetFlagWhenUnlimited(t *testing.T) {
	t.Parallel()
	bin, argsFile, _ := fakeClaude(t, []string{lineResult}, "")
	if _, err := newClaude(bin).Run(context.Background(), baseReq(t)); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "--max-budget-usd") || strings.Contains(string(args), "--model") {
		t.Fatalf("unexpected flags:\n%s", args)
	}
}

func TestClaudeBudgetSubtype(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":1.01,"usage":{"input_tokens":5,"output_tokens":5}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil || res.Status != StatusBudgetExceeded || res.Usage.CostUSD != 1.01 {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestClaudeResultDurationParsed(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"ok":true},"duration_ms":83250,"duration_api_ms":61000,"total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil || res.Status != StatusOK {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if want := 83250 * time.Millisecond; res.Duration != want {
		t.Fatalf("Duration = %v, want %v", res.Duration, want)
	}
}

func TestClaudeResultWithoutDurationIsZero(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil || res.Status != StatusOK {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if res.Duration != 0 {
		t.Fatalf("Duration = %v, want 0", res.Duration)
	}
}

func TestClaudeErrorResult(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusError || !strings.Contains(res.Error, "boom") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeNoStructuredOutput(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"success","is_error":false,"result":"just prose","usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusError || !strings.Contains(res.Error, "structured output") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeJSONResultTextFallback(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"success","is_error":false,"result":"{\"a\":1}","usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusOK || string(res.Output) != `{"a":1}` {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeExitWithoutResult(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit}, "echo 'not logged in' >&2; exit 1")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusError || !strings.Contains(res.Error, "not logged in") || !strings.Contains(res.Error, "code 1") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudePermissionDenialsWarn(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"x":1},"usage":{},"permission_denials":[{"tool_name":"Bash"}]}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusOK || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "denied") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeStringMessageEventIsNotUnparseable(t *testing.T) {
	t.Parallel()
	denied := `{"type":"system","subtype":"permission_denied","message":"Permission to use Bash was denied"}`
	r := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"x":1},"usage":{}}`
	bin, _, _ := fakeClaude(t, []string{lineInit, denied, r}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeInvalidJSONLineIsUnparseable(t *testing.T) {
	t.Parallel()
	r := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"x":1},"usage":{}}`
	bin, _, _ := fakeClaude(t, []string{"not json", r}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 unparseable") {
		t.Fatalf("result = %+v", res)
	}
}

// Budget enforcement: the caller cancels from OnUsage and the process is killed.
func TestClaudeCancelledFromOnUsage(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit, lineAsst1}, "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := baseReq(t)
	req.OnUsage = func(d budget.Usage) {
		if d.Tokens(budget.CountBillable) > 100 {
			cancel()
		}
	}
	start := time.Now()
	res, err := newClaude(bin).Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", res.Status, res.Error)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("process not killed promptly")
	}
	if res.Usage.CacheWriteTokens != 5000 {
		t.Fatalf("live usage lost: %+v", res.Usage)
	}
}

func TestClaudeRequiresLogDir(t *testing.T) {
	t.Parallel()
	if _, err := newClaude("claude").Run(context.Background(), Request{Role: RoleImplementer}); err == nil {
		t.Fatal("missing LogDir accepted")
	}
}

func TestRoleSettingsReadOnly(t *testing.T) {
	t.Parallel()
	b, err := RoleSettings([]string{"Read", "Glob", "Grep"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Permissions.Allow, ",") != "Read,Glob,Grep" {
		t.Fatalf("allow = %v", s.Permissions.Allow)
	}
	for _, a := range s.Permissions.Allow {
		if a == "Edit" || a == "Write" || a == "Bash" {
			t.Fatalf("read-only role allowed %s", a)
		}
	}
}

// The final usage correction can cross a limit after claude already finished; the
// finished result must win over the cancellation it triggers.
func TestClaudeFinishedRunSurvivesLateCancel(t *testing.T) {
	t.Parallel()
	small := `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":2,"cache_creation_input_tokens":100,"output_tokens":5}}}`
	result := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"ok":true},"total_cost_usd":0.02,"usage":{"input_tokens":2,"cache_creation_input_tokens":100,"output_tokens":900}}`
	bin, _, _ := fakeClaude(t, []string{small, result}, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := baseReq(t)
	var spent budget.Usage
	req.OnUsage = func(d budget.Usage) {
		spent = spent.Add(d)
		if over, _ := budget.Exceeded(spent, budget.Limits{Tokens: 500}, budget.CountBillable); over {
			cancel()
		}
	}
	res, err := newClaude(bin).Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || string(res.Output) != `{"ok":true}` {
		t.Fatalf("finished run reported as %s (%s)", res.Status, res.Error)
	}
	if ctx.Err() == nil {
		t.Fatal("test did not exercise the late cancel")
	}
}

func TestClaudeReportsStartedProcess(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineResult}, "")
	req := baseReq(t)
	var got []Started
	req.OnStart = func(s Started) { got = append(got, s) }
	res, err := newClaude(bin).Run(context.Background(), req)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if len(got) != 1 {
		t.Fatalf("OnStart called %d times", len(got))
	}
	if got[0].PID <= 0 || got[0].Command != filepath.Base(bin) || got[0].RawPath != res.RawPath || got[0].RawPath == "" {
		t.Fatalf("started = %+v (bin %s, raw %s)", got[0], bin, res.RawPath)
	}
}

func TestClaudeKilledRunEstimatesCostFromInitModel(t *testing.T) {
	t.Parallel()
	initOpus := `{"type":"system","subtype":"init","model":"claude-opus-5-5[1m]"}`
	bin, _, _ := fakeClaude(t, []string{initOpus, lineAsst1}, "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := baseReq(t)
	req.Model = "haiku" // the init event's model wins over the request's
	req.OnUsage = func(budget.Usage) { cancel() }
	res, err := newClaude(bin).Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	want := (2*4.0 + 10*20.0 + 5000*8.0) / 1e6
	if res.Status != StatusCancelled || !res.CostEstimated || res.Usage.CostUSD < want-1e-9 || res.Usage.CostUSD > want+1e-9 {
		t.Fatalf("result = %+v, want estimated cost %v", res, want)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "cost estimated from tokens (claude reported none)") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestClaudeEstimateFallsBackToRequestModelAndOverrides(t *testing.T) {
	t.Parallel()
	noModel := `{"type":"system","subtype":"init"}`
	bin, _, _ := fakeClaude(t, []string{noModel, lineAsst1}, "exit 1")
	c := newClaude(bin)
	c.Prices = map[string]price.Rates{"sonnet": {CacheWrite1h: 1000}}
	req := baseReq(t)
	req.Model = "sonnet"
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := 5000 * 1000.0 / 1e6
	if !res.CostEstimated || res.Usage.CostUSD < want-1e-9 || res.Usage.CostUSD > want+1e-9 {
		t.Fatalf("result = %+v, want cost %v", res, want)
	}
}

func TestClaudeUnknownModelLeavesCostZeroAndWarns(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit, lineAsst1}, "exit 1")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.CostEstimated || res.Usage.CostUSD != 0 {
		t.Fatalf("result = %+v, want no estimate", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "could not be estimated") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestClaudeReportedCostIsNotEstimated(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{`{"type":"system","subtype":"init","model":"opus"}`, lineAsst1, lineResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.CostEstimated || res.Usage.CostUSD != 0.0125 {
		t.Fatalf("result = %+v, want reported cost 0.0125", res)
	}
}

func writeStream(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "implementer.stream.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageFromStreamDeduplicatesAssistantUsageWithoutResult(t *testing.T) {
	t.Parallel()
	p := writeStream(t, lineInit, lineAsst1, lineAsst1b, lineAsst2)
	u, model, finished, err := UsageFromStream(p)
	if err != nil {
		t.Fatal(err)
	}
	want := budget.Usage{InputTokens: 6, OutputTokens: 30, CacheWriteTokens: 5100, CacheReadTokens: 5000}
	if u != want || model != "claude-haiku" || finished {
		t.Fatalf("got %+v, %q, finished=%v; want %+v, claude-haiku, false", u, model, finished, want)
	}
}

func TestUsageFromStreamResultIsAuthoritative(t *testing.T) {
	t.Parallel()
	p := writeStream(t, lineInit, lineAsst1, lineAsst2, lineResult)
	u, model, finished, err := UsageFromStream(p)
	if err != nil {
		t.Fatal(err)
	}
	want := budget.Usage{InputTokens: 6, OutputTokens: 90, CacheWriteTokens: 5100, CacheReadTokens: 5000, CostUSD: 0.0125}
	if u != want || model != "claude-haiku" || !finished {
		t.Fatalf("got %+v, %q, finished=%v; want %+v, claude-haiku, true", u, model, finished, want)
	}
}

func TestUsageFromStreamMissingFileFails(t *testing.T) {
	t.Parallel()
	if _, _, _, err := UsageFromStream(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing stream accepted")
	}
}

func TestLastActionDescribesLatestToolCallOrText(t *testing.T) {
	t.Parallel()
	tool := func(name, input string) string {
		return `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	text := func(s string) string {
		return `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"` + s + `"}]}}` + "\n"
	}
	result := `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}` + "\n"
	cases := []struct{ name, stream, want string }{
		{"bash", text("hi") + tool("Bash", `{"command":"go test ./..."}`) + result, "Bash go test ./..."},
		{"read", tool("Read", `{"file_path":"internal/x.go"}`), "Read internal/x.go"},
		{"grep", tool("Grep", `{"pattern":"func Foo"}`), "Grep func Foo"},
		{"text last", tool("Bash", `{"command":"ls"}`) + text("All done"), "All done"},
		{"truncated last line", tool("Read", `{"file_path":"a.go"}`) + `{"type":"assistant","message":{"content":[{"type":"tool_use","na`, "Read a.go"},
		{"nothing yet", `{"type":"system","subtype":"init"}` + "\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(c.stream), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := LastAction(path)
			if err != nil || got != c.want {
				t.Fatalf("LastAction = %q, %v; want %q", got, err, c.want)
			}
		})
	}
	if _, err := LastAction(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: want an error")
	}
}

// Lines observed on a real run that hit the 5-hour usage limit.
const (
	lineLimitWarn     = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1795348200,"rateLimitType":"five_hour","utilization":0.99,"overageStatus":"rejected","isUsingOverage":false,"surpassedThreshold":0.9}}`
	lineLimitRejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1795348200,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false}}`
	lineLimitText     = `{"type":"assistant","message":{"id":"m9","content":[{"type":"text","text":"You've hit your 5-hour limit · resets 12:50pm (Europe/Berlin)"}]}}`
	lineLimitResult   = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"terminal_reason":"api_error","result":"You've hit your 5-hour limit · resets 12:50pm (Europe/Berlin)","total_cost_usd":0,"usage":{}}`
)

func TestClaudeRateLimitedRunReportsLimitAndReset(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit, lineAsst1, lineLimitWarn, lineLimitRejected, lineLimitText, lineLimitResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Unix(1795348200, 0)
	if res.Status != StatusRateLimited || res.LimitType != "five_hour" || !res.ResetsAt.Equal(reset) {
		t.Fatalf("got %q %q %v", res.Status, res.LimitType, res.ResetsAt)
	}
	if !strings.Contains(res.Error, "five_hour") || !strings.Contains(res.Error, reset.Local().Format(localTimeFormat)) {
		t.Fatalf("error = %q", res.Error)
	}
	if res.Usage.InputTokens != 2 {
		t.Fatalf("usage not kept: %+v", res.Usage)
	}
}

func TestClaudeRateLimitedWithoutEventHasUnknownReset(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit, lineLimitResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRateLimited || res.LimitType != "" || !res.ResetsAt.IsZero() || res.Error == "" {
		t.Fatalf("got %+v", res)
	}
}

func TestClaudeRateLimitedOnRejectedEventWithOtherError(t *testing.T) {
	t.Parallel()
	errRes := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"usage":{}}`
	bin, _, _ := fakeClaude(t, []string{lineLimitRejected, errRes}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusRateLimited || res.LimitType != "five_hour" {
		t.Fatalf("got %+v", res)
	}
}

func TestClaudeUsageWarningAddedOnceAndKeepsSuccess(t *testing.T) {
	t.Parallel()
	bin, _, _ := fakeClaude(t, []string{lineInit, lineLimitWarn, lineLimitWarn, lineResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "usage at 99% of the five_hour limit, resets at " + time.Unix(1795348200, 0).Local().Format(localTimeFormat)
	if res.Status != StatusOK || len(res.Warnings) != 1 || res.Warnings[0] != want {
		t.Fatalf("status %q warnings %q, want %q", res.Status, res.Warnings, want)
	}
}

func TestClaudeOtherErrorResultStaysError(t *testing.T) {
	t.Parallel()
	errRes := `{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"overloaded","usage":{}}`
	bin, _, _ := fakeClaude(t, []string{lineLimitWarn, errRes}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusError {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestUsageFromStreamHandlesRateLimitedStream(t *testing.T) {
	t.Parallel()
	path := writeStream(t, lineInit, lineAsst1, lineLimitWarn, lineLimitRejected, lineLimitText, lineLimitResult)
	u, model, finished, err := UsageFromStream(path)
	if err != nil || !finished || model != "claude-haiku" || u.InputTokens != 2 {
		t.Fatalf("u=%+v model=%q finished=%v err=%v", u, model, finished, err)
	}
}

// lostHost starts a worker that writes only the init event, then reports what the herdr host
// reports when the worker's pane was closed: a signalled exit with an error.
type lostHost struct{}

func (lostHost) Name() string { return "lost" }

func (lostHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	if err := os.WriteFile(spec.StdoutPath, []byte(lineInit+"\n"), 0o644); err != nil {
		return nil, err
	}
	return lostHandle{}, nil
}

type lostHandle struct{}

func (lostHandle) PID() int    { return 1 }
func (lostHandle) Kill() error { return nil }
func (lostHandle) Wait() (host.Exit, error) {
	return host.Exit{Code: -1, Signaled: true}, errors.New("the herdr pane running the worker was closed or killed; the attempt is lost")
}

func TestClaudeRecordsHostErrorWhenWorkerPaneIsClosed(t *testing.T) {
	t.Parallel()
	c := NewClaude("claude", lostHost{})
	c.Poll = 10 * time.Millisecond
	res, err := c.Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusError || !strings.Contains(res.Error, "herdr pane running the worker was closed or killed") {
		t.Fatalf("result = %+v, want the closed-pane error recorded", res)
	}
}

// recordingHost records the spec it was asked to start and whether the worker's TMPDIR existed
// at that moment, then writes a successful result.
type recordingHost struct {
	mu        sync.Mutex
	spec      host.ProcSpec
	tmpExists bool
}

func (*recordingHost) Name() string { return "recording" }

func (r *recordingHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spec = spec
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			if fi, err := os.Stat(v); err == nil && fi.IsDir() {
				r.tmpExists = true
			}
		}
	}
	if err := os.WriteFile(spec.StdoutPath, []byte(lineResult+"\n"), 0o644); err != nil {
		return nil, err
	}
	return okHandle{}, nil
}

type okHandle struct{}

func (okHandle) PID() int                 { return 1 }
func (okHandle) Kill() error              { return nil }
func (okHandle) Wait() (host.Exit, error) { return host.Exit{}, nil }

func runRecorded(t *testing.T, req Request) *recordingHost {
	t.Helper()
	rh := &recordingHost{}
	c := NewClaude("claude", rh)
	c.Poll = 10 * time.Millisecond
	res, err := c.Run(context.Background(), req)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("run = %+v, %v", res, err)
	}
	return rh
}

func TestWorkerTMPDIRIsInsideAttemptRunDirAndExistsBeforeStart(t *testing.T) {
	t.Parallel()
	req := baseReq(t)
	rh := runRecorded(t, req)
	var got []string
	for _, kv := range rh.spec.Env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0] != filepath.Join(req.LogDir, "tmp") {
		t.Fatalf("TMPDIR entries = %v, want one pointing to %s", got, filepath.Join(req.LogDir, "tmp"))
	}
	if !rh.tmpExists {
		t.Fatalf("TMPDIR %s did not exist when the worker started", got[0])
	}
}

func TestWorkerTMPDIROverridesInheritedValue(t *testing.T) {
	t.Parallel()
	req := baseReq(t)
	req.Env = []string{"PATH=/usr/bin", "TMPDIR=/inherited/tmp", "HOME=/home/x", "TMPDIR=/again"}
	rh := runRecorded(t, req)
	want := []string{"PATH=/usr/bin", "HOME=/home/x", "TMPDIR=" + filepath.Join(req.LogDir, "tmp")}
	if strings.Join(rh.spec.Env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env = %q, want %q", rh.spec.Env, want)
	}
	if strings.Join(req.Env, ",") != "PATH=/usr/bin,TMPDIR=/inherited/tmp,HOME=/home/x,TMPDIR=/again" {
		t.Fatalf("request env was modified: %q", req.Env)
	}
}

func TestWorkerTMPDIRCreateFailureNamesPath(t *testing.T) {
	t.Parallel()
	req := baseReq(t)
	// A file where the tmp directory should go makes creating it fail.
	tmp := filepath.Join(req.LogDir, "tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewClaude("claude", &recordingHost{}).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), tmp) {
		t.Fatalf("err = %v, want an error naming %s", err, tmp)
	}
}

func TestRoleSettingsDenyProcessKillersForEveryRole(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{RolePlanner, RoleImplementer, RoleVerifier, RoleResolver, RoleFixer} {
		req := baseReq(t)
		req.Role = role
		runRecorded(t, req)
		b, err := os.ReadFile(filepath.Join(req.LogDir, string(role)+".settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Permissions struct {
				Deny []string `json:"deny"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		deny := map[string]bool{}
		for _, d := range s.Permissions.Deny {
			deny[d] = true
		}
		for _, want := range []string{"Bash(pkill:*)", "Bash(killall:*)", "Bash(pgrep:*)"} {
			if !deny[want] {
				t.Errorf("%s settings deny = %v, missing %s", role, s.Permissions.Deny, want)
			}
		}
	}
}
