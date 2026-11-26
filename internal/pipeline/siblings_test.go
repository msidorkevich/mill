package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

func TestSiblingTasksReachVerifierPromptOfGoalTask(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if _, stop, err := e.p.Verify(context.Background(), "g1/a"); err != nil || stop != "" {
		t.Fatalf("Verify: stop %q, err %v", stop, err)
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 2 {
		t.Fatalf("verifier prompts = %d, want 2 (run and Verify)", len(ps))
	}
	for i, p := range ps {
		_, sib, _ := strings.Cut(p, "## Other tasks of this goal")
		sib, _, _ = strings.Cut(sib, "## Gate results")
		for _, w := range []string{"g1/b: Task b", "Owns: b.txt", "Summary: create b.txt", "out of scope"} {
			if !strings.Contains(sib, w) {
				t.Fatalf("verifier prompt %d lacks %q in the sibling section:\n%s", i+1, w, p)
			}
		}
		if strings.Contains(sib, "g1/a") {
			t.Fatalf("verifier prompt %d lists the task itself as a sibling:\n%s", i+1, p)
		}
	}
}

func TestSiblingSectionAbsentForStandaloneTask(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "solo")
	if out := e.run(t, "solo"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 1 || strings.Contains(ps[0], "Other tasks of this goal") {
		t.Fatalf("verifier prompts = %q", ps)
	}
}

func TestSiblingSummaryIsFirstSentenceCappedAt120(t *testing.T) {
	t.Parallel()
	if got := specSummary("Add  a flag.\nThen document it."); got != "Add a flag." {
		t.Errorf("first sentence: got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := specSummary(long); got != long[:120] {
		t.Errorf("long spec: got %d chars", len(got))
	}
}
