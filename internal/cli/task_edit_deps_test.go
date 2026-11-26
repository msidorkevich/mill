package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// specB is task b of fanInPlan with the given depends_on.
func specB(deps string) string {
	return `{"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
  "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": ` + deps + `, "size": "S"}`
}

// planDeps returns the depends_on of local task id in goal g1's stored plan.
func planDeps(t *testing.T, s *store.Store, id string) []string {
	t.Helper()
	g, err := s.GetGoal(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse(g.Plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range p.Tasks {
		if pt.ID == id {
			return pt.DependsOn
		}
	}
	t.Fatalf("task %s not in plan", id)
	return nil
}

func TestTaskEditAddsDependencyToTaskWithoutBranch(t *testing.T) {
	t.Parallel()
	for _, st := range []string{store.TaskPending, store.TaskBlocked, store.TaskFailed} {
		t.Run(st, func(t *testing.T) {
			t.Parallel()
			root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
			setStatus(t, root, "g1/b", st)
			r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["a"]`)))
			if r.code != ExitOK || !strings.Contains(r.stdout, "Added dependencies g1/a to g1/b.") ||
				!strings.Contains(r.stdout, "mill run g1 --retry-blocked") {
				t.Fatalf("edit: %+v", r)
			}
			s := openStore(t, root)
			deps, err := s.TaskDeps(context.Background(), "g1/b")
			if err != nil || !reflect.DeepEqual(deps, []string{"g1/a"}) {
				t.Errorf("deps = %v, %v", deps, err)
			}
			if got := planDeps(t, s, "b"); !reflect.DeepEqual(got, []string{"a"}) {
				t.Errorf("plan depends_on = %v", got)
			}
			if tk, _ := s.GetTask(context.Background(), "g1/b"); tk.Status != st {
				t.Errorf("status = %s, want %s", tk.Status, st)
			}
		})
	}
}

func TestTaskEditAddedDependencyMovesBranchOntoIt(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "g1/b")
	a, _ := s.GetBranch(ctx, "g1/a")
	aHead := gitC(t, root, "rev-parse", a.Name)
	setStatus(t, root, "g1/b", store.TaskBlocked)

	r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["a"]`)))
	if r.code != ExitOK || !strings.Contains(r.stdout, "Added dependencies g1/a to g1/b.") ||
		!strings.Contains(r.stdout, "onto "+a.Name) || !strings.Contains(r.stdout, short(before.StartSHA)+" -> "+short(aHead)) {
		t.Fatalf("edit: %+v", r)
	}
	after, err := s.GetBranch(ctx, "g1/b")
	if err != nil {
		t.Fatal(err)
	}
	if after.StartSHA != aHead || after.HeadSHA == before.HeadSHA {
		t.Errorf("branch row = %+v, a head %s", after, aHead)
	}
	if head := gitC(t, after.Worktree, "rev-parse", "HEAD"); head != after.HeadSHA {
		t.Errorf("head = %s, recorded %s", head, after.HeadSHA)
	}
	gitC(t, after.Worktree, "merge-base", "--is-ancestor", aHead, "HEAD")
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(after.Worktree, f)); err != nil {
			t.Errorf("worktree lacks %s", f)
		}
	}
	evs, _ := s.TaskEvents(ctx, "g1/b")
	found := false
	for _, e := range evs {
		found = found || e.Kind == "task-deps-added"
	}
	if !found {
		t.Errorf("no task-deps-added event: %+v", evs)
	}
}

func TestTaskEditAddedDependencyConflictBlocksTask(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	ctx := context.Background()
	s := openStore(t, root)
	a, _ := s.GetBranch(ctx, "g1/a")
	if err := os.WriteFile(filepath.Join(a.Worktree, "b.txt"), []byte("from a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, a.Worktree, "add", "b.txt")
	gitC(t, a.Worktree, "commit", "-q", "-m", "a writes b.txt")
	before, _ := s.GetBranch(ctx, "g1/b")
	setStatus(t, root, "g1/b", store.TaskFailed)

	r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["a"]`)))
	if r.code != ExitError || !strings.Contains(r.stderr, "b.txt") || !strings.Contains(r.stderr, "mill/g1/a") {
		t.Fatalf("edit: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g1/b")
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "b.txt") || !strings.Contains(tk.Reason, "mill/g1/a") ||
		!strings.Contains(tk.Reason, "resolve by hand") {
		t.Errorf("task = %s %q", tk.Status, tk.Reason)
	}
	if deps, _ := s.TaskDeps(ctx, "g1/b"); !reflect.DeepEqual(deps, []string{"g1/a"}) {
		t.Errorf("deps = %v; the dependency should stay added", deps)
	}
	if after, _ := s.GetBranch(ctx, "g1/b"); after != before {
		t.Errorf("branch row changed: %+v -> %+v", before, after)
	}
}

func TestTaskEditRefusesDroppedOrReorderedDependencies(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/c", store.TaskBlocked)
	base := `{"id": "c", "title": "C", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["c.txt"], "depends_on": ["a", "b"], "size": "S"}`
	for name, deps := range map[string]string{"drop": `["a"]`, "reorder": `["b", "a"]`, "replace": `["a", "x"]`} {
		r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, strings.Replace(base, `["a", "b"]`, deps, 1)))
		if r.code != ExitError || !strings.Contains(r.stderr, "depends_on cannot drop or reorder") ||
			!strings.Contains(r.stderr, "published stacks and branches depend on them") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if deps, _ := openStore(t, root).TaskDeps(context.Background(), "g1/c"); !reflect.DeepEqual(deps, []string{"g1/a", "g1/b"}) {
		t.Errorf("deps = %v", deps)
	}
}

func TestTaskEditRefusesDependencyOnStandaloneTask(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	one := `{"id": "solo", "title": "Solo", "kind": "docs", "spec": "write docs", "acceptance": [], "checks": [], "owns": ["d.md"], "size": "S"}`
	if err := openStore(t, root).CreateTask(context.Background(), mustSpec(t, one)); err != nil {
		t.Fatal(err)
	}
	f := writeFile(t, strings.Replace(one, `"size"`, `"depends_on": ["other"], "size"`, 1))
	r := run(t, root, nil, "task", "edit", "solo", "--file", f)
	if r.code != ExitError || !strings.Contains(r.stderr, "standalone task has no dependencies") {
		t.Fatalf("edit: %+v", r)
	}
}

func TestTaskEditRefusesCycleAndKeepsEditFile(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/a", store.TaskBlocked)
	ed := filepath.Join(t.TempDir(), "ed.sh")
	script := "#!/bin/sh\nsed -e 's/\"depends_on\": \\[\\]/\"depends_on\": [\"c\"]/' -e 's/\"title\": \"A\"/\"title\": \"Cyclic\"/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n"
	if err := os.WriteFile(ed, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runEnv(t, root, Env{Getenv: fakeGetenv(map[string]string{"EDITOR": ed})}, "task", "edit", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "not valid") || !strings.Contains(r.stderr, "cycle") || !strings.Contains(r.stderr, "kept in ") {
		t.Fatalf("edit: %+v", r)
	}
	i := strings.Index(r.stderr, "kept in ")
	kept := strings.Fields(r.stderr[i+len("kept in "):])[0]
	defer os.Remove(kept)
	if data, err := os.ReadFile(kept); err != nil || !strings.Contains(string(data), `"c"`) {
		t.Errorf("kept file = %q, %v", data, err)
	}
	s := openStore(t, root)
	if deps, _ := s.TaskDeps(context.Background(), "g1/a"); len(deps) != 0 {
		t.Errorf("deps = %v", deps)
	}
	if tk, _ := s.GetTask(context.Background(), "g1/a"); tk.Title != "A" {
		t.Errorf("task changed: title %q", tk.Title)
	}
}

func TestTaskEditRefusesUnknownDependency(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/b", store.TaskBlocked)
	r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["nope"]`)))
	if r.code != ExitError || !strings.Contains(r.stderr, "not valid") || !strings.Contains(r.stderr, "g1/nope") {
		t.Fatalf("edit: %+v", r)
	}
	if deps, _ := openStore(t, root).TaskDeps(context.Background(), "g1/b"); len(deps) != 0 {
		t.Errorf("deps = %v", deps)
	}
}

func TestTaskEditRefusesRunningTask(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/b", store.TaskRunning)
	r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["a"]`)))
	if r.code != ExitError || !strings.Contains(r.stderr, "running") {
		t.Errorf("running: %+v", r)
	}
	if deps, _ := openStore(t, root).TaskDeps(context.Background(), "g1/b"); len(deps) != 0 {
		t.Errorf("deps = %v", deps)
	}
}

func TestTaskEditHoldsClaimWhileEditing(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/b", store.TaskBlocked)
	db := project.PathsAt(root).DB
	snap := t.TempDir()
	// The editor copies the database while the edit is open, then adds dependency a.
	ed := filepath.Join(t.TempDir(), "ed.sh")
	script := "#!/bin/sh\nset -e\nfor f in \"" + db + "\"*; do cp \"$f\" \"" + snap + "/\"; done\n" +
		"sed 's/\"depends_on\": \\[\\]/\"depends_on\": [\"a\"]/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n"
	if err := os.WriteFile(ed, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runEnv(t, root, Env{Getenv: fakeGetenv(map[string]string{"EDITOR": ed})}, "task", "edit", "g1/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Added dependencies g1/a to g1/b.") {
		t.Fatalf("edit: %+v", r)
	}
	ctx := context.Background()
	held, err := store.Open(ctx, filepath.Join(snap, filepath.Base(db)), store.Options{
		ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	claims, err := held.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := claims["g1/b"]; !ok || c.Command != "task edit" {
		t.Errorf("claims during the edit = %+v; want g1/b claimed by task edit", claims)
	}
	after, err := openStore(t, root).ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["g1/b"]; ok {
		t.Errorf("claim not released: %+v", after)
	}
}
