package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// resumeSetup runs task "r" until it is blocked in verify by the task budget: the implementer
// spends 0.4 of a 0.6 budget and the verifier may use 0.25. The gate counts its runs in the
// returned file. Raising the task budget to 0.8 lets the verifier run.
func resumeSetup(t *testing.T, checkCmd string, script runner.Script, tweak func(*config.Config)) (*env, string) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "gate-runs")
	e := setup(t, "echo run >> '"+counter+"' && "+checkCmd, script, func(c *config.Config) {
		stepBudgets(0.6, 0)(c)
		if tweak != nil {
			tweak(c)
		}
	})
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.p.Now = func() time.Time { return clock }
	e.task(t, "r")
	out := e.run(t, "r")
	tk, err := e.p.Store.GetTask(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.TaskBlocked || tk.Phase != store.PhaseVerify {
		t.Fatalf("first run = %+v in phase %s, want blocked in verify\n%s", out, tk.Phase, e.out)
	}
	e.p.Cfg.Budget.Task = budget.Limits{USD: 5}
	return e, counter
}

func resumeScript(verdicts ...runner.Step) runner.Script {
	return runner.Script{
		runner.RoleImplementer: {
			impl(map[string]string{"hello.txt": "hello\n"}, budget.Usage{CostUSD: 0.4}),
			impl(map[string]string{"hello.txt": "hello again\n"}),
		},
		runner.RoleVerifier: verdicts,
	}
}

func lastAttempt(t *testing.T, e *env, id string) store.Attempt {
	t.Helper()
	atts, err := e.p.Store.Attempts(context.Background(), id)
	if err != nil || len(atts) == 0 {
		t.Fatalf("attempts = %+v, %v", atts, err)
	}
	return atts[len(atts)-1]
}

func TestRetryAfterVerifierBudgetBlockResumesAtTheVerifier(t *testing.T) {
	t.Parallel()
	e, counter := resumeSetup(t, "grep -q hello hello.txt", resumeScript(verdict("pass")), nil)
	callsBefore := len(e.fake.Calls())
	gatesBefore := len(gateReuseGateEvents(t, e.p.Store, "r"))
	if gateReuseRuns(t, counter) != 1 || gatesBefore != 1 {
		t.Fatalf("first run gate runs = %d, gate events = %d; want 1, 1", gateReuseRuns(t, counter), gatesBefore)
	}

	out := e.run(t, "r")
	if out.Status != store.TaskVerified {
		t.Fatalf("retry = %+v\n%s", out, e.out)
	}
	calls := e.fake.Calls()[callsBefore:]
	if len(calls) != 1 || calls[0].Role != runner.RoleVerifier {
		t.Fatalf("retry runner calls = %+v, want exactly one verifier call", calls)
	}
	if !strings.Contains(e.out.String(), "r a1 resuming at the verifier (gate passed at ") {
		t.Fatalf("output lacks the resume line:\n%s", e.out)
	}
	if !strings.Contains(calls[0].Prompt, "grep -q hello hello.txt") {
		t.Fatalf("verifier prompt lacks the recorded gate result:\n%s", calls[0].Prompt)
	}
	if a := lastAttempt(t, e, "r"); a.N != 1 || a.Role != string(runner.RoleVerifier) {
		t.Fatalf("last attempt = %+v, want the verifier of attempt 1", a)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d time(s), want 1 (none on the resumed path)", n)
	}
	if n := len(gateReuseGateEvents(t, e.p.Store, "r")); n != gatesBefore {
		t.Fatalf("gate events = %d, want %d", n, gatesBefore)
	}
}

func TestResumedVerifierRejectionStartsNextAttemptWithFindings(t *testing.T) {
	t.Parallel()
	e, _ := resumeSetup(t, "grep -q hello hello.txt", resumeScript(verdict("fail", "say hello again"), verdict("pass")), nil)
	callsBefore := len(e.fake.Calls())

	out := e.run(t, "r")
	if out.Status != store.TaskVerified {
		t.Fatalf("retry = %+v\n%s", out, e.out)
	}
	if !strings.Contains(e.out.String(), "resuming at the verifier") {
		t.Fatalf("output lacks the resume line:\n%s", e.out)
	}
	var roles []string
	for _, c := range e.fake.Calls()[callsBefore:] {
		roles = append(roles, string(c.Role))
	}
	if strings.Join(roles, ",") != "verifier,implementer,verifier" {
		t.Fatalf("retry roles = %v, want verifier, implementer, verifier", roles)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if !strings.Contains(ip[1], "say hello again") {
		t.Fatalf("next implementer prompt lacks the verifier's findings:\n%s", ip[1])
	}
	if a := lastAttempt(t, e, "r"); a.N != 2 {
		t.Fatalf("last attempt = %+v, want attempt 2", a)
	}
}

func TestRetryAfterHeadMovedStartsImplementer(t *testing.T) {
	t.Parallel()
	e, _ := resumeSetup(t, "grep -q hello hello.txt", resumeScript(verdict("pass")), nil)
	ctx := context.Background()
	br, err := e.p.Store.GetBranch(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(br.Worktree, "extra.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, br.Worktree, "a commit after the block"); err != nil {
		t.Fatal(err)
	}
	assertRetryImplements(t, e)
}

func TestRetryWithDirtyWorktreeStartsImplementer(t *testing.T) {
	t.Parallel()
	e, _ := resumeSetup(t, "grep -q hello hello.txt", resumeScript(verdict("pass")), nil)
	br, err := e.p.Store.GetBranch(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(br.Worktree, "extra.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertRetryImplements(t, e)
}

func TestRetryBlockedInImplementOrGateStartsImplementer(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{store.PhaseImplement, store.PhaseGate} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			e, _ := resumeSetup(t, "grep -q hello hello.txt", resumeScript(verdict("pass")), nil)
			// The gate passed on the head; only the phase the task was blocked in differs.
			if err := e.p.Store.SetTaskStatus(context.Background(), "r", store.TaskBlocked, phase, "blocked in "+phase); err != nil {
				t.Fatal(err)
			}
			assertRetryImplements(t, e)
		})
	}
}

func TestRetryWithoutRecordedPassedGateStartsImplementer(t *testing.T) {
	t.Parallel()
	// The gate fails on its first run and passes on the re-run: the branch records the gate
	// as passed, but the attempt's task-gate event holds the failed run.
	marker := filepath.Join(t.TempDir(), "flaky")
	check := "grep -q hello hello.txt && if [ -f '" + marker + "' ]; then true; else touch '" + marker + "'; false; fi"
	e, _ := resumeSetup(t, check, resumeScript(verdict("pass")), func(c *config.Config) { c.GateRerun = true })
	br, err := e.p.Store.GetBranch(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if br.GateSHA == "" || br.GateSHA != br.HeadSHA {
		t.Fatalf("branch = %+v, want the gate recorded as passed on the head", br)
	}
	assertRetryImplements(t, e)
}

// assertRetryImplements re-runs task "r" and checks that it started a new implementer attempt
// instead of resuming at the verifier.
func assertRetryImplements(t *testing.T, e *env) {
	t.Helper()
	callsBefore := len(e.fake.Calls())
	out := e.run(t, "r")
	if out.Status != store.TaskVerified {
		t.Fatalf("retry = %+v\n%s", out, e.out)
	}
	if strings.Contains(e.out.String(), "resuming at the verifier") {
		t.Fatalf("retry resumed at the verifier:\n%s", e.out)
	}
	calls := e.fake.Calls()[callsBefore:]
	if len(calls) == 0 || calls[0].Role != runner.RoleImplementer {
		t.Fatalf("retry runner calls = %+v, want an implementer first", calls)
	}
	if a := lastAttempt(t, e, "r"); a.N != 2 {
		t.Fatalf("last attempt = %+v, want attempt 2", a)
	}
}
