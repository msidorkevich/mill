package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// modelTask creates a task with a size, complexity and optional model override.
func (e *env) modelTask(t *testing.T, id, size, complexity, model string) {
	t.Helper()
	spec := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "create hello.txt",
		Acceptance: []string{"hello.txt says hello"}, Owns: []string{}, Size: size, Complexity: complexity, Model: model}
	if err := e.p.Store.CreateTask(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}

// attemptModels lists "role n model (reason)" for each recorded attempt, in order.
func (e *env) attemptModels(t *testing.T, id string) []string {
	t.Helper()
	atts, err := e.p.Store.Attempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range atts {
		out = append(out, fmt.Sprintf("%s %d %s (%s)", a.Role, a.N, a.Model, a.ModelReason))
	}
	return out
}

// callModels lists "role model" for each runner call, in order.
func callModels(f *runner.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, string(c.Role)+" "+c.Model)
	}
	return out
}

func wantList(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestRoleModelSmallLowTaskRunsHaikuImplementerAndSonnetVerifier(t *testing.T) {
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.modelTask(t, "small", "S", "low", "")
	if out := e.run(t, "small"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	wantList(t, "runner models", callModels(e.fake), "implementer haiku", "verifier sonnet")
	wantList(t, "attempt models", e.attemptModels(t, "small"),
		"implementer 1 haiku (mapping S/low)", "verifier 1 sonnet (verifier floor sonnet)")

	// A separate verification also runs on the implementer's tier, raised to the floor.
	if _, stop, err := e.p.Verify(context.Background(), "small"); err != nil || stop != "" {
		t.Fatalf("Verify: %v, stop %q\n%s", err, stop, e.out)
	}
	got := e.attemptModels(t, "small")
	wantList(t, "verify attempt model", got[2:], "verifier 2 sonnet (verifier floor sonnet)")
}

func TestRoleModelOverrideRunsImplementerWithReasonOverride(t *testing.T) {
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.modelTask(t, "pinned", "S", "low", "claude-opus-5-5")
	if out := e.run(t, "pinned"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	wantList(t, "runner models", callModels(e.fake), "implementer claude-opus-5-5", "verifier claude-opus-5-5")
	wantList(t, "attempt models", e.attemptModels(t, "pinned"),
		"implementer 1 claude-opus-5-5 (override)", "verifier 1 claude-opus-5-5 (implementer tier)")
}

func TestEscalatesOneTierAfterFailedVerification(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "helo\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("fail", "typo: helo"), verdict("pass")},
	}, nil)
	e.modelTask(t, "esc", "S", "low", "")
	if out := e.run(t, "esc"); out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	wantList(t, "runner models", callModels(e.fake),
		"implementer haiku", "verifier sonnet", "implementer sonnet", "verifier sonnet")
	wantList(t, "attempt models", e.attemptModels(t, "esc"),
		"implementer 1 haiku (mapping S/low)", "verifier 1 sonnet (verifier floor sonnet)",
		"implementer 2 sonnet (escalated after attempt 1 failed)", "verifier 2 sonnet (implementer tier)")
}

func TestEscalationStopsAtOpus(t *testing.T) {
	e := setup(t, `false`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}), impl(map[string]string{"a": "2"}),
			impl(map[string]string{"a": "3"}), impl(map[string]string{"a": "4"})},
	}, func(c *config.Config) { c.MaxAttempts = 4 })
	e.modelTask(t, "cap", "S", "medium", "")
	if out := e.run(t, "cap"); out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	wantList(t, "attempt models", e.attemptModels(t, "cap"),
		"implementer 1 sonnet (mapping S/medium)",
		"implementer 2 opus (escalated after attempt 1 failed)",
		"implementer 3 opus (escalated after attempt 2 failed)",
		"implementer 4 opus (escalated after attempt 3 failed)")
}

func TestEscalationContinuesAcrossRuns(t *testing.T) {
	e := setup(t, `false`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}), impl(map[string]string{"a": "2"})},
	}, func(c *config.Config) { c.MaxAttempts = 1 })
	e.modelTask(t, "again", "S", "low", "")
	for i := 0; i < 2; i++ {
		if out := e.run(t, "again"); out.Status != store.TaskBlocked {
			t.Fatalf("run %d outcome = %+v\n%s", i+1, out, e.out)
		}
	}
	wantList(t, "attempt models", e.attemptModels(t, "again"),
		"implementer 1 haiku (mapping S/low)", "implementer 2 sonnet (escalated after attempt 1 failed)")
}

func TestRoleModelResolverRunsOnTaskBaseModel(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "helo\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("fail", "typo: helo"), verdict("pass")},
		runner.RoleResolver:    {resolved()},
	}, nil)
	e.modelTask(t, "res", "S", "low", "")
	if out := e.run(t, "res"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	out, err := e.p.Resolve(context.Background(), "res", resolverInput(), t.TempDir())
	if err != nil || out.Stop != "" || out.Reason != "" {
		t.Fatalf("Resolve: %v, %+v\n%s", err, out, e.out)
	}
	calls := callModels(e.fake)
	wantList(t, "resolver call", calls[len(calls)-1:], "resolver haiku")
	got := e.attemptModels(t, "res")
	wantList(t, "resolver attempt", got[len(got)-1:], "resolver 3 haiku (mapping S/low)")
}

func TestRoleModelMissingMappingBlocksInImplement(t *testing.T) {
	e := setup(t, `true`, runner.Script{}, func(c *config.Config) {
		c.Models = map[string]map[string]string{"S": {"medium": "sonnet"}}
	})
	e.modelTask(t, "nomap", "S", "low", "")
	out := e.run(t, "nomap")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, `add low = "..." to [models.S]`) {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	tk, _ := e.p.Store.GetTask(context.Background(), "nomap")
	if tk.Phase != store.PhaseImplement {
		t.Fatalf("phase = %q, want %q", tk.Phase, store.PhaseImplement)
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("runner calls = %d, want 0", n)
	}
}

func TestEscalatedAttemptsExhaustTaskBudgetAndBlock(t *testing.T) {
	e := setup(t, "false", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}, budget.Usage{CostUSD: 0.4}),
			impl(map[string]string{"a": "2"}, budget.Usage{CostUSD: 0.4})},
	}, func(c *config.Config) {
		c.MaxAttempts = 5
		c.Budget.Attempt = budget.Limits{USD: 0.5}
		c.Budget.Size = nil
		c.Budget.Task = budget.Limits{USD: 1.2}
	})
	e.modelTask(t, "spent", "S", "low", "")
	out := e.run(t, "spent")
	// Two attempts spend 0.8; a third may need 0.5 but only 0.4 is left.
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "task budget cannot cover another attempt") ||
		!strings.Contains(out.Reason, "of $1.20, an attempt may use $0.50") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	wantList(t, "runner models", callModels(e.fake), "implementer haiku", "implementer sonnet")
	for _, c := range e.fake.Calls() {
		if c.Budget.USD != 0.5 {
			t.Fatalf("%s attempt budget = %v, want the unchanged 0.5", c.Role, c.Budget)
		}
	}
}
