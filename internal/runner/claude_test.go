package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mill/internal/budget"
	"mill/internal/host"
	"mill/internal/price"
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
	r := `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":1.01,"usage":{"input_tokens":5,"output_tokens":5}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil || res.Status != StatusBudgetExceeded || res.Usage.CostUSD != 1.01 {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestClaudeErrorResult(t *testing.T) {
	r := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusError || !strings.Contains(res.Error, "boom") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeNoStructuredOutput(t *testing.T) {
	r := `{"type":"result","subtype":"success","is_error":false,"result":"just prose","usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusError || !strings.Contains(res.Error, "structured output") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeJSONResultTextFallback(t *testing.T) {
	r := `{"type":"result","subtype":"success","is_error":false,"result":"{\"a\":1}","usage":{}}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusOK || string(res.Output) != `{"a":1}` {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeExitWithoutResult(t *testing.T) {
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
	r := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"x":1},"usage":{},"permission_denials":[{"tool_name":"Bash"}]}`
	bin, _, _ := fakeClaude(t, []string{r}, "")
	res, _ := newClaude(bin).Run(context.Background(), baseReq(t))
	if res.Status != StatusOK || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "denied") {
		t.Fatalf("result = %+v", res)
	}
}

func TestClaudeStringMessageEventIsNotUnparseable(t *testing.T) {
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
	if _, err := newClaude("claude").Run(context.Background(), Request{Role: RoleImplementer}); err == nil {
		t.Fatal("missing LogDir accepted")
	}
}

func TestRoleSettingsReadOnly(t *testing.T) {
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
	bin, _, _ := fakeClaude(t, []string{`{"type":"system","subtype":"init","model":"opus"}`, lineAsst1, lineResult}, "")
	res, err := newClaude(bin).Run(context.Background(), baseReq(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.CostEstimated || res.Usage.CostUSD != 0.0125 {
		t.Fatalf("result = %+v, want reported cost 0.0125", res)
	}
}
