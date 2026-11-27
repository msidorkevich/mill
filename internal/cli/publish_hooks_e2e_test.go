package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// stackPlan is a two-task stack: b depends on a.
const stackPlan = `{
  "goal": "Stack",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": ["a"], "size": "S"}
  ]
}`

// hooksStackRepo makes a repo cloned from a bare remote whose goal g1 (the stack plan) has
// run to verified with the fake runner. on_publish and on_update append one line per run to
// <dir>/hooks.log: "<event> <task_id> <base_branch> <previous_base or ->", read from the
// document on stdin. A hook prints "boom" and exits 4 while <dir>/fail-<event> exists.
func hooksStackRepo(t *testing.T) (root, bare, dir string) {
	t.Helper()
	dir = t.TempDir()
	log := filepath.Join(dir, "hooks.log")
	script := `d=$(cat); f() { v=$(printf '%s' "$d" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"); echo "${v:--}"; }; ` +
		`echo "$(f event) $(f task_id) $(f base_branch) $(f previous_base)" >> '` + log + `'; ` +
		`if [ -f '` + dir + `'/fail-"$MILL_HOOK_EVENT" ]; then echo boom; exit 4; fi`

	bare = filepath.Join(t.TempDir(), "remote.git")
	gitC(t, t.TempDir(), "init", "-q", "--bare", "--initial-branch=main", bare)
	seed := gitRepo(t)
	gitC(t, seed, "checkout", "-q", "-b", "main")
	gitC(t, seed, "commit", "-q", "--allow-empty", "-m", "initial")
	gitC(t, seed, "remote", "add", "origin", bare)
	gitC(t, seed, "push", "-q", "origin", "main")

	root = filepath.Join(t.TempDir(), "work")
	gitC(t, t.TempDir(), "clone", "-q", bare, root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "true"
max_attempts = 1
max_parallel = 2
worktree_root = "` + t.TempDir() + `"
on_publish = '''` + script + `'''
on_update = '''` + script + `'''
hook_timeout = "30s"

[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	fake := `{"tasks": {
  "g1/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `
}}`
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(fake), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(context.Background(), root, "setup"); err != nil {
		t.Fatal(err)
	}
	gitC(t, root, "push", "-q", "origin", "main")
	if r := groupBRun(t, root, "plan", "--file", writePlan(t, stackPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := groupBRun(t, root, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve plan: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	return root, bare, dir
}

// hookLogLines is every line the hooks appended to <dir>/hooks.log, in order.
func hookLogLines(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "hooks.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// approveStackAndLandParent approves the goal, checks that on_publish ran parent then child,
// then merges review/g1/a into main on the remote.
func approveStackAndLandParent(t *testing.T, root, bare, dir string) {
	t.Helper()
	r := groupBRun(t, root, "review", "approve", "--goal", "g1")
	if r.code != ExitOK {
		t.Fatalf("approve --goal: %+v", r)
	}
	want := []string{"publish g1/a main -", "publish g1/b review/g1/a -"}
	if got := hookLogLines(t, dir); !slices.Equal(got, want) {
		t.Fatalf("hook runs after approve --goal = %q, want %q", got, want)
	}
	forgeMerge(t, bare, func(f string) {
		gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g1/a", "origin/review/g1/a")
	})
}

func TestPublishHooksRunInStackOrderAndUpdateCarriesNewBase(t *testing.T) {
	t.Parallel()
	root, bare, dir := hooksStackRepo(t)
	approveStackAndLandParent(t, root, bare, dir)
	bPub := remoteRefs(t, bare)["refs/heads/review/g1/b"]

	r := groupBRun(t, root, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	if strings.Contains(r.stderr, "mill hook run") {
		t.Errorf("sync printed a hook failure: %s", r.stderr)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g1/b"]; got == bPub {
		t.Error("sync did not re-publish review/g1/b after its parent landed")
	}
	want := []string{"publish g1/a main -", "publish g1/b review/g1/a -", "update g1/b main review/g1/a"}
	if got := hookLogLines(t, dir); !slices.Equal(got, want) {
		t.Errorf("hook runs after sync = %q, want %q", got, want)
	}
	s := openStore(t, root)
	for id, st := range map[string]string{"g1/a": store.TaskLanded, "g1/b": store.TaskPublished} {
		if tk, err := s.GetTask(context.Background(), id); err != nil || tk.Status != st {
			t.Errorf("%s = %+v, %v; want %s", id, tk, err, st)
		}
	}
}

// processStderr runs fn with os.Stderr redirected to a temporary file and returns what was
// written to it. Publishing from approve, sync and keep reports hook failures on the
// process's stderr, not on Env.Stderr. Its callers must not be parallel, because os.Stderr
// is shared by the whole test binary.
func processStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	os.Stderr = f
	func() {
		defer func() { os.Stderr = old }()
		fn()
	}()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Not parallel: it captures the process's stderr (see processStderr).
func TestPublishHooksFailingUpdateExitsZeroWithRerunHint(t *testing.T) {
	root, bare, dir := hooksStackRepo(t)
	approveStackAndLandParent(t, root, bare, dir)
	fail := filepath.Join(dir, "fail-update")
	if err := os.WriteFile(fail, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var r result
	stderr := processStderr(t, func() { r = groupBRun(t, root, "sync") })
	if r.code != ExitOK {
		t.Fatalf("sync with a failing on_update = %d, want %d: %+v", r.code, ExitOK, r)
	}
	stderr += r.stderr
	for _, want := range []string{"hook on_update for g1/b failed (exit 4): boom", "re-run it with `mill hook run g1/b`"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("sync stderr lacks %q:\n%s", want, stderr)
		}
	}
	if got := hookLogLines(t, dir); len(got) != 3 || got[2] != "update g1/b main review/g1/a" {
		t.Errorf("hook runs after sync = %q, want the update of g1/b last", got)
	}
	if tk, err := openStore(t, root).GetTask(context.Background(), "g1/b"); err != nil || tk.Status != store.TaskPublished {
		t.Errorf("g1/b = %+v, %v; want published", tk, err)
	}

	show := groupBRun(t, root, "task", "show", "g1/b")
	if show.code != ExitOK {
		t.Fatalf("task show: %+v", show)
	}
	if !strings.Contains(show.stdout, "hook update: exit 4 (") || !strings.Contains(show.stdout, "): boom") {
		t.Errorf("task show lacks the failed update run:\n%s", show.stdout)
	}
	if !strings.Contains(show.stdout, "hook publish: ok (") {
		t.Errorf("task show lacks the publish run:\n%s", show.stdout)
	}

	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	r = groupBRun(t, root, "hook", "run", "g1/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hook on_update for g1/b succeeded") {
		t.Fatalf("hook run after the failure: %+v", r)
	}
	if got := hookLogLines(t, dir); len(got) != 4 || !strings.HasPrefix(got[3], "update g1/b main ") {
		t.Errorf("hook runs after hook run = %q, want a fourth update of g1/b onto main", got)
	}
	show = groupBRun(t, root, "task", "show", "g1/b")
	if show.code != ExitOK || !strings.Contains(show.stdout, "hook update: ok (") {
		t.Errorf("task show after the re-run lacks the successful update:\n%+v", show)
	}
}
