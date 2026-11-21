package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mill/internal/gitx"
	"mill/internal/project"
	"mill/internal/runlock"
	"mill/internal/store"
)

// fanInPlan has two independent tasks and one that depends on both.
const fanInPlan = `{
  "goal": "Fan in",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": [], "size": "S"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "write c.txt", "acceptance": ["all files exist"],
     "checks": ["test -f a.txt && test -f b.txt && test -f c.txt"], "owns": ["c.txt"], "depends_on": ["a", "b"], "size": "S"}
  ]
}`

// taskSteps is a per-task fake script: the implementer writes files, the verifier passes.
func taskSteps(files string) string {
	return `{"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": ` + files + `}],
             "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]}`
}

// goalRepo makes an initialized repo with a commit on main, the given fake script and extra
// config, and the fan-in plan approved as g1.
func goalRepo(t *testing.T, script, extraCfg string) string {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "true"
max_attempts = 1
max_parallel = 2
worktree_root = "` + t.TempDir() + `"
` + extraCfg + `
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "plan", "--file", writePlan(t, fanInPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	return root
}

func fanInScript(aFiles string) string {
	return `{"tasks": {
  "g1/a": ` + taskSteps(aFiles) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `
}}`
}

func TestRunGoalFanInVerifiesAllTasks(t *testing.T) {
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	r := run(t, root, nil, "run", "g1")
	if r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"g1/a: started", "g1/b: started", "g1/c: started", "g1/c: verified",
		"goal g1: done", "ID  STATUS", "Done: all tasks are verified, on branches mill/g1/*."} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"g1/a", "g1/b", "g1/c"} {
		tk, err := s.GetTask(ctx, id)
		if err != nil || tk.Status != store.TaskVerified {
			t.Errorf("%s = %+v, %v", id, tk, err)
		}
		if !gitx.BranchExists(ctx, root, "mill/"+id) {
			t.Errorf("branch mill/%s missing", id)
		}
	}
	br, err := s.GetBranch(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(br.Worktree, f)); err != nil {
			t.Errorf("fan-in worktree lacks %s: %v", f, err)
		}
	}
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalDone {
		t.Errorf("goal = %+v", g)
	}

	if again := run(t, root, nil, "run", "g1"); again.code != ExitError || !strings.Contains(again.stderr, "already done") {
		t.Errorf("run of done goal: %+v", again)
	}
}

func TestRunGoalBlockedTaskExitsNotVerified(t *testing.T) {
	// a writes the wrong file, so its check fails and it blocks after one attempt.
	root := goalRepo(t, fanInScript(`{"wrong.txt": "x\n"}`), "")
	r := run(t, root, nil, "run", "g1")
	if r.code != ExitNotVerified {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"Blocked: g1/a blocked (", "waiting: g1/c (needs g1/a)",
		"status    blocked", "mill run g1 --retry-blocked"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[string]string{"g1/a": store.TaskBlocked, "g1/b": store.TaskVerified, "g1/c": store.TaskPending} {
		if tk, err := s.GetTask(ctx, id); err != nil || tk.Status != want {
			t.Errorf("%s = %+v, %v; want %s", id, tk, err, want)
		}
	}
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalBlocked || !strings.Contains(g.Reason, "g1/a blocked") {
		t.Errorf("goal = %+v", g)
	}
}

func TestRunGoalRefusesPublishedStartMode(t *testing.T) {
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), `start_children_on = "published"`)
	r := run(t, root, nil, "run", "g1")
	if r.code != ExitError || !strings.Contains(r.stderr, `start_children_on = "verified"`) {
		t.Fatalf("run: %+v", r)
	}
}

func TestRunGoalRefusedWhileRunLockHeld(t *testing.T) {
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	r := run(t, root, nil, "run", "g1")
	if r.code != ExitError || !strings.Contains(r.stderr, "another mill run is active") {
		t.Fatalf("run while locked: %+v", r)
	}
}

func TestRunGoalRefusesDraftAndUsageErrors(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	if r := run(t, root, nil, "run", "g1"); r.code != ExitError || !strings.Contains(r.stderr, "mill plan approve g1") {
		t.Fatalf("draft: %+v", r)
	}
	if r := run(t, root, nil, "run"); r.code != ExitUsage || !strings.Contains(r.stderr, "mill run <goal>") {
		t.Fatalf("no goal: %+v", r)
	}
	if r := run(t, root, nil, "run", "g1", "--bogus"); r.code != ExitUsage {
		t.Fatalf("bad flag: %+v", r)
	}
}
