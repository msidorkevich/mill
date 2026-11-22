package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

func resolved(usage ...budget.Usage) runner.Step {
	return runner.Step{Output: json.RawMessage(`{"summary":"kept both","resolved":["hello.txt"]}`), Usage: usage}
}

func resolverInput() prompt.ResolverInput {
	return prompt.ResolverInput{Conflicts: []prompt.ConflictFile{{Path: "hello.txt", Hunks: "<<<<<<< HEAD\nhi\n=======\nhello\n>>>>>>> task\n"}}, Onto: "main"}
}

func TestResolveRecordsResolverAttemptWithUsage(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolved(budget.Usage{InputTokens: 42, CostUSD: 0.02})},
	}, nil)
	e.task(t, "keep")
	dir := t.TempDir()
	ctx := context.Background()
	out, err := e.p.Resolve(ctx, "keep", resolverInput(), dir)
	if err != nil {
		t.Fatalf("Resolve: %v\n%s", err, e.out)
	}
	if out.Stop != "" || out.Reason != "" || out.Status != string(runner.StatusOK) || out.Output.Summary != "kept both" ||
		len(out.Output.Resolved) != 1 || out.Attempt != 1 || out.Usage.InputTokens != 42 {
		t.Fatalf("outcome = %+v", out)
	}
	atts, _ := e.p.Store.Attempts(ctx, "keep")
	if len(atts) != 1 || atts[0].Role != "resolver" || atts[0].N != 1 || atts[0].Status != "ok" ||
		atts[0].Usage.InputTokens != 42 || atts[0].Usage.CostUSD != 0.02 {
		t.Fatalf("attempts = %+v", atts)
	}
	calls := e.fake.Calls()
	if len(calls) != 1 || calls[0].Dir != dir || strings.Join(calls[0].Tools, ",") != strings.Join(ImplementerTools, ",") ||
		!strings.Contains(calls[0].Prompt, "<<<<<<< HEAD") || !strings.HasSuffix(calls[0].LogDir, "a1") {
		t.Fatalf("calls = %+v", calls)
	}
	tk, _ := e.p.Store.GetTask(ctx, "keep")
	if tk.Status != store.TaskPending {
		t.Fatalf("task status = %s, want unchanged", tk.Status)
	}
}

func TestResolveOverAttemptBudgetReturnsStop(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolved(budget.Usage{InputTokens: 600}, budget.Usage{InputTokens: 600})},
	}, func(c *config.Config) {
		c.Budget.Attempt = budget.Limits{Tokens: 1000}
		c.Budget.Size = nil
	})
	e.task(t, "keep")
	out, err := e.p.Resolve(context.Background(), "keep", resolverInput(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stop, "budget exceeded in attempt 1 (resolver)") {
		t.Fatalf("outcome = %+v", out)
	}
	atts, _ := e.p.Store.Attempts(context.Background(), "keep")
	if len(atts) != 1 || atts[0].Status != "budget_exceeded" || atts[0].Usage.InputTokens != 1200 {
		t.Fatalf("attempts = %+v", atts)
	}
}

func TestResolveWithExhaustedTaskBudgetStopsBeforeRunning(t *testing.T) {
	e := setup(t, "true", runner.Script{runner.RoleResolver: {resolved()}}, func(c *config.Config) {
		c.Budget.Attempt = budget.Limits{USD: 0.5}
		c.Budget.Size = nil
		c.Budget.Task = budget.Limits{USD: 0.1}
	})
	e.task(t, "keep")
	out, err := e.p.Resolve(context.Background(), "keep", resolverInput(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stop, "task budget cannot cover another attempt") || len(e.fake.Calls()) != 0 {
		t.Fatalf("outcome = %+v, calls = %d", out, len(e.fake.Calls()))
	}
}

func TestResolveRateLimitedReturnsStop(t *testing.T) {
	resets := time.Date(2026, 11, 22, 12, 50, 0, 0, time.Local)
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {{RateLimit: &runner.RateLimit{Type: "five_hour", ResetsAt: resets}}},
	}, nil)
	e.p.Now = func() time.Time { return time.Date(2026, 11, 22, 10, 0, 0, 0, time.Local) }
	e.task(t, "keep")
	out, err := e.p.Resolve(context.Background(), "keep", resolverInput(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !out.RateLimited || !out.ResetsAt.Equal(resets) || !strings.HasPrefix(out.Stop, "rate limited: the five_hour usage limit") {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestResolveFailedRunReturnsReasonNotError(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {{Error: "agent crashed"}, {Output: json.RawMessage(`{"summary":"","resolved":[]}`)}},
	}, nil)
	e.task(t, "keep")
	ctx := context.Background()
	out, err := e.p.Resolve(ctx, "keep", resolverInput(), t.TempDir())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Stop != "" || !strings.Contains(out.Reason, "agent crashed") || out.Status != string(runner.StatusError) {
		t.Fatalf("outcome = %+v", out)
	}
	out, err = e.p.Resolve(ctx, "keep", resolverInput(), t.TempDir())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Stop != "" || !strings.Contains(out.Reason, "no summary") || out.Attempt != 2 {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestRunAfterResolveStartsAtNextAttemptNumber(t *testing.T) {
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleResolver:    {resolved()},
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "keep")
	ctx := context.Background()
	if _, err := e.p.Resolve(ctx, "keep", resolverInput(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	atts, _ := e.p.Store.Attempts(ctx, "keep")
	if len(atts) != 3 || atts[0].Role != "resolver" || atts[0].N != 1 || atts[1].Role != "implementer" || atts[1].N != 2 || atts[2].N != 2 {
		t.Fatalf("attempts = %+v", atts)
	}
}

func TestVerifyReturnsVerdictAndKeepsTaskStatus(t *testing.T) {
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("fail", "lost the greeting")},
	}, nil)
	e.task(t, "keep")
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	v, stop, err := e.p.Verify(ctx, "keep")
	if err != nil || stop != "" {
		t.Fatalf("Verify: %v, stop %q\n%s", err, stop, e.out)
	}
	if v.Verdict != "fail" || len(v.Findings) != 1 || v.Findings[0].Issue != "lost the greeting" {
		t.Fatalf("verdict = %+v", v)
	}
	atts, _ := e.p.Store.Attempts(ctx, "keep")
	if len(atts) != 3 || atts[2].Role != "verifier" || atts[2].N != 2 {
		t.Fatalf("attempts = %+v", atts)
	}
	vp := prompts(e.fake, runner.RoleVerifier)
	if len(vp) != 2 || !strings.Contains(vp[1], "+hello") {
		t.Fatalf("verifier prompt:\n%s", vp[len(vp)-1])
	}
	evs := e.events(t, "keep", "task-verdict")
	if len(evs) != 2 || evs[1]["attempt"] != float64(2) {
		t.Fatalf("task-verdict events = %+v", evs)
	}
	br, _ := e.p.Store.GetBranch(ctx, "keep")
	if evs[1]["head"] != br.HeadSHA {
		t.Fatalf("verdict head = %v, branch head %s", evs[1]["head"], br.HeadSHA)
	}
	tk, _ := e.p.Store.GetTask(ctx, "keep")
	if tk.Status != store.TaskVerified {
		t.Fatalf("task status = %s, want verified (unchanged)", tk.Status)
	}
}

func TestVerifyRateLimitedReturnsStop(t *testing.T) {
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass"), {RateLimit: &runner.RateLimit{}}},
	}, nil)
	e.task(t, "keep")
	e.run(t, "keep")
	_, stop, err := e.p.Verify(context.Background(), "keep")
	if err != nil || !strings.HasPrefix(stop, "rate limited:") {
		t.Fatalf("Verify: %v, stop %q", err, stop)
	}
	if evs := e.events(t, "keep", "task-verdict"); len(evs) != 1 {
		t.Fatalf("task-verdict events = %d, want 1", len(evs))
	}
}

func TestRecordGateFailureFindingsReachNextImplementerPrompt(t *testing.T) {
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}), impl(map[string]string{"hello.txt": "hello again\n"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.task(t, "keep")
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	g := gate.Result{Passed: false, Commands: []gate.CmdResult{
		{Cmd: "make lint", Exit: 0},
		{Cmd: "make test", Exit: 2, Tail: "FAIL TestHello"},
	}}
	if err := e.p.RecordGateFailure(ctx, "keep", g); err != nil {
		t.Fatal(err)
	}
	evs := e.events(t, "keep", "attempt-findings")
	if len(evs) != 1 || evs[0]["attempt"] != float64(2) {
		t.Fatalf("attempt-findings events = %+v", evs)
	}
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if len(ip) != 2 || !strings.Contains(ip[1], "[gate] `make test` failed (exit 2)") || !strings.Contains(ip[1], "FAIL TestHello") ||
		strings.Contains(ip[1], "make lint") {
		t.Fatalf("implementer prompt after the gate failure:\n%s", ip[len(ip)-1])
	}
}
