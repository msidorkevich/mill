package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// stepBudgets allows an attempt 0.5 USD and reserves half of that, 0.25, for the verifier.
func stepBudgets(task, goal float64) func(*config.Config) {
	return func(c *config.Config) {
		c.Budget.Attempt = budget.Limits{USD: 0.5}
		c.Budget.Size = nil
		c.Budget.VerifierShare = 0.5
		c.Budget.Task = budget.Limits{USD: task}
		c.Budget.Goal = budget.Limits{USD: goal}
	}
}

func spendingScript() runner.Script {
	return runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}, budget.Usage{CostUSD: 0.4})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}
}

func verifierCalls(f *runner.Fake) int {
	return len(prompts(f, runner.RoleVerifier))
}

func TestVerifierRunsWhenBudgetCoversItsShareButNotAnAttempt(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", spendingScript(), stepBudgets(0.8, 0))
	e.task(t, "tight")
	// 0.4 is left after the implementer: less than an attempt, more than the verifier's share.
	if out := e.run(t, "tight"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if n := verifierCalls(e.fake); n != 1 {
		t.Fatalf("verifier calls = %d, want 1", n)
	}
}

func TestTaskBudgetBelowVerifierShareBlocksInVerify(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", spendingScript(), stepBudgets(0.6, 0))
	e.task(t, "short")
	out := e.run(t, "short")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "task budget cannot cover the verifier: spent ") ||
		!strings.Contains(out.Reason, "the verifier may use") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	tk, err := e.p.Store.GetTask(context.Background(), "short")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskBlocked || tk.Phase != store.PhaseVerify {
		t.Fatalf("task = %s in %s, want blocked in verify", tk.Status, tk.Phase)
	}
	if n := verifierCalls(e.fake); n != 0 {
		t.Fatalf("verifier calls = %d, want 0", n)
	}
}

func TestGoalBudgetReservesOnlyVerifierShareBeforeVerifier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		goal  float64
		want  string
		calls int
	}{
		{"covers the share", 0.8, store.TaskVerified, 1},
		{"below the share", 0.6, store.TaskBlocked, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, "true", spendingScript(), stepBudgets(0, tc.goal))
			e.goal(t, nil)
			out := e.run(t, "g1/a")
			if out.Status != tc.want || verifierCalls(e.fake) != tc.calls {
				t.Fatalf("outcome = %+v, verifier calls = %d\n%s", out, verifierCalls(e.fake), e.out)
			}
			if tc.want != store.TaskBlocked {
				return
			}
			if !strings.Contains(out.Reason, "goal budget exhausted for g1: spent ") || !strings.Contains(out.Reason, "the verifier may use") {
				t.Fatalf("reason = %q", out.Reason)
			}
			tk, err := e.p.Store.GetTask(context.Background(), "g1/a")
			if err != nil {
				t.Fatal(err)
			}
			if tk.Phase != store.PhaseVerify {
				t.Fatalf("phase = %s, want verify", tk.Phase)
			}
		})
	}
}

func TestVerifyReservesOnlyVerifierShare(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", spendingScript(), stepBudgets(0.8, 0))
	e.task(t, "again")
	if out := e.run(t, "again"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()

	// 0.4 is left: not enough for an attempt, enough for the verifier.
	v, stop, err := e.p.Verify(ctx, "again")
	if err != nil || stop != "" || v.Verdict != "pass" || verifierCalls(e.fake) != 2 {
		t.Fatalf("Verify = %+v, stop %q, err %v, verifier calls = %d", v, stop, err, verifierCalls(e.fake))
	}

	e.p.Cfg.Budget.Task = budget.Limits{USD: 0.6} // 0.2 left
	v, stop, err = e.p.Verify(ctx, "again")
	if err != nil || v.Verdict != "" || !strings.Contains(stop, "task budget cannot cover the verifier") || verifierCalls(e.fake) != 2 {
		t.Fatalf("Verify = %+v, stop %q, err %v, verifier calls = %d", v, stop, err, verifierCalls(e.fake))
	}
}
