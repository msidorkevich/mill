package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// mccSetup is mergeCheckSetup with a second goal whose tasks a, b (depending on a) and c (no
// dependencies) own files in pkg/. The merge check command appends a line to the returned
// file each time it runs.
func mccSetup(t *testing.T) (*mergeCheckEnv, string) {
	t.Helper()
	ran := filepath.Join(t.TempDir(), "ran")
	script := runner.FakeScript{Tasks: map[string]runner.Script{}}
	e := mergeCheckSetup(t, "echo ran >> "+ran, script)
	ctx := context.Background()
	pl := plan.Plan{Goal: "contained", Assumptions: []string{}}
	for _, id := range []string{"a", "b", "c"} {
		deps := []string{}
		if id == "b" {
			deps = []string{"a"}
		}
		pl.Tasks = append(pl.Tasks, taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "do " + id,
			Acceptance: []string{"works"}, Checks: []string{}, Owns: []string{"pkg/" + id + ".go"}, Size: "S", DependsOn: deps})
	}
	g, err := e.s.CreateGoal(ctx, "contained", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	e.goal = g.ID
	for _, id := range []string{"a", "b", "c"} {
		script.Tasks[e.id(id)] = mcTask(map[string]string{"pkg/" + id + ".go": "package pkg\n"})
	}
	e.p.Runner = runner.NewFake(script)
	// Slot rows are deleted on release, so log every row ever taken.
	for _, q := range []string{
		`CREATE TABLE mcc_slots (task_id TEXT NOT NULL)`,
		`CREATE TRIGGER mcc_slot_taken AFTER INSERT ON gate_slots BEGIN INSERT INTO mcc_slots VALUES (NEW.task_id); END`,
	} {
		if _, err := e.s.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return e, ran
}

// mccSlots is the number of gate slot rows the task has taken.
func mccSlots(t *testing.T, e *mergeCheckEnv, short string) int {
	t.Helper()
	var n int
	if err := e.s.DB().QueryRow(`SELECT COUNT(*) FROM mcc_slots WHERE task_id = ?`, e.id(short)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mccVerify(t *testing.T, e *mergeCheckEnv, short string) {
	t.Helper()
	if out := e.run(t, short); out.Status != store.TaskVerified {
		t.Fatalf("%s outcome = %+v, want verified\n%s", short, out, e.out)
	}
}

func TestMergeCheckSkipsContainedDependency(t *testing.T) {
	t.Parallel()
	e, ran := mccSetup(t)
	mccVerify(t, e, "a")
	mccVerify(t, e, "b")

	br, err := e.s.GetBranch(context.Background(), e.id("b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), e.root, "cat-file", "-e", br.HeadSHA+":pkg/a.go"); err != nil {
		t.Fatalf("b is not built on a: %v", err)
	}
	if strings.Contains(e.out.String(), "merge check") {
		t.Errorf("b was merge-checked with its dependency:\n%s", e.out)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("the merge check command ran (stat err %v)", err)
	}
	// The parent of the throwaway worktrees outlives them: absent means none was created.
	parent := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), ".merge-check")
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Errorf("a merge check worktree was created under %s (stat err %v)", parent, err)
	}
	// b's own gate takes the only slot row.
	if n := mccSlots(t, e, "b"); n != 1 {
		t.Errorf("b took %d gate slot(s), want 1 (its gate only)", n)
	}
	logs, err := filepath.Glob(filepath.Join(e.p.Paths.Runs, e.id("b"), "*", "merge-check"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Errorf("merge check gate results were recorded: %v", logs)
	}
}

func TestMergeCheckStillChecksSiblingNotContained(t *testing.T) {
	t.Parallel()
	e, ran := mccSetup(t)
	mccVerify(t, e, "a")
	mccVerify(t, e, "c")
	if err := os.Remove(ran); err != nil {
		t.Fatalf("c was not merge-checked with a: %v\n%s", err, e.out)
	}
	// b contains a but not c: only c is merged.
	mccVerify(t, e, "b")
	if want := e.id("b") + " a1 merge check with " + e.id("c") + ":"; !strings.Contains(e.out.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, e.out)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Errorf("the merge check command did not run for b: %v", err)
	}
	if n := mccSlots(t, e, "b"); n != 2 {
		t.Errorf("b took %d gate slot(s), want 2 (gate and merge check)", n)
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckAncestryErrorFailsCheck(t *testing.T) {
	t.Parallel()
	e, ran := mccSetup(t)
	mccVerify(t, e, "a")
	// With a's branch ref gone, git cannot decide whether c contains it. a's recorded head
	// is still a commit, so listing its changed files works.
	br, err := e.s.GetBranch(context.Background(), e.id("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), e.root, "update-ref", "-d", "refs/heads/"+br.Name); err != nil {
		t.Fatal(err)
	}
	out, err := e.p.Run(context.Background(), e.id("c"))
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether") || !strings.Contains(err.Error(), e.id("a")) {
		t.Fatalf("Run c = %+v, %v, want an ancestry error naming %s\n%s", out, err, e.id("a"), e.out)
	}
	if got := e.status(t, "c").Status; out.Status != store.TaskFailed || got != store.TaskFailed {
		t.Errorf("c outcome = %s, task = %s, want failed", out.Status, got)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("the merge check command ran (stat err %v)", err)
	}
}
