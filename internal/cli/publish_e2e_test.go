package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// e2ePlan has a, b (depends on a), c and d; c and d are independent of everything.
const e2ePlan = `{
  "goal": "Publish and merge",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": ["a"], "size": "S"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "write c.txt", "acceptance": ["c.txt exists"],
     "checks": ["test -f c.txt"], "owns": ["c.txt"], "depends_on": [], "size": "S"},
    {"id": "d", "title": "D", "kind": "implement", "spec": "write d.txt", "acceptance": ["d.txt exists"],
     "checks": ["test -f d.txt"], "owns": ["d.txt"], "depends_on": [], "size": "S"}
  ]
}`

// e2eRepo makes a repo cloned from a bare remote, with the fake runner scripted per task and
// the e2e plan approved as g1.
func e2eRepo(t *testing.T) (root, bare string) {
	t.Helper()
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

[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	script := `{"tasks": {
  "g1/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `,
  "g1/d": ` + taskSteps(`{"d.txt": "d\n"}`) + `
}}`
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(context.Background(), root, "setup"); err != nil {
		t.Fatal(err)
	}
	gitC(t, root, "push", "-q", "origin", "main")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, e2ePlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve plan: %+v", r)
	}
	return root, bare
}

// forgeMerge plays the forge: it clones the remote, lets merge change main in the clone,
// pushes main and returns main's new tip.
func forgeMerge(t *testing.T, bare string, merge func(dir string)) string {
	t.Helper()
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	merge(f)
	gitC(t, f, "push", "-q", "origin", "main")
	return gitC(t, f, "rev-parse", "HEAD")
}

func TestE2EPublishReviewAndMergeDetectionAgainstBareRemote(t *testing.T) {
	root, bare := e2eRepo(t)
	ctx := context.Background()
	s := openStore(t, root)
	mainTip := remoteRefs(t, bare)["refs/heads/main"]

	// assertMain checks that main only moved through the forge helper, and that mill pushed
	// nothing but review/* refs.
	assertMain := func() {
		t.Helper()
		if got := remoteRefs(t, bare)["refs/heads/main"]; got != mainTip {
			t.Errorf("remote main = %s, want %s", got, mainTip)
		}
		for name := range remoteRefs(t, bare) {
			if name != "refs/heads/main" && !strings.HasPrefix(name, "refs/heads/review/") {
				t.Errorf("unexpected remote ref %s", name)
			}
		}
	}
	syncOK := func() string {
		t.Helper()
		r := run(t, root, nil, "sync")
		if r.code != ExitOK {
			t.Fatalf("sync: %+v", r)
		}
		assertMain()
		return r.stdout
	}
	status := func(id string) string {
		t.Helper()
		tk, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return tk.Status
	}
	wantLanded := func(id, method, commit string) {
		t.Helper()
		if st := status(id); st != store.TaskLanded {
			t.Errorf("%s status = %s, want landed", id, st)
		}
		if ev := lastEvent(t, s, id, "task-landed"); ev["method"] != method || ev["commit"] != commit {
			t.Errorf("%s task-landed = %v, want method %s commit %s", id, ev, method, commit)
		}
	}

	if r := run(t, root, nil, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}

	// 1. Publishing: b is refused until a is published; then all four are pushed, stacked.
	r := run(t, root, nil, "review", "approve", "g1/b")
	if r.code != ExitError || !strings.Contains(r.stderr, "dependencies must be published or landed") {
		t.Fatalf("approve b before a: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/b"]; ok {
		t.Fatal("refused approve pushed review/g1/b")
	}
	for _, id := range []string{"g1/a", "g1/b", "g1/c", "g1/d"} {
		if r := run(t, root, nil, "review", "approve", id); r.code != ExitOK {
			t.Fatalf("approve %s: %+v", id, r)
		}
	}
	refs := remoteRefs(t, bare)
	for _, n := range []string{"a", "b", "c", "d"} {
		if refs["refs/heads/review/g1/"+n] == "" {
			t.Errorf("remote has no review/g1/%s", n)
		}
	}
	aPub, bPub := refs["refs/heads/review/g1/a"], refs["refs/heads/review/g1/b"]
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", bPub); got != bPub+" "+aPub {
		t.Errorf("b's published parents = %q, want a's commit %s", got, aPub)
	}
	assertMain()

	// 2. Merge commit for a: landed by ancestor; b restacked onto main and re-published.
	mainTip = forgeMerge(t, bare, func(f string) {
		gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g1/a", "origin/review/g1/a")
	})
	syncOK()
	wantLanded("g1/a", "ancestor", mainTip)
	newB := remoteRefs(t, bare)["refs/heads/review/g1/b"]
	if newB == bPub {
		t.Error("review/g1/b was not re-published after a landed")
	}
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", newB); got != newB+" "+mainTip {
		t.Errorf("republished b parents = %q, want main tip %s", got, mainTip)
	}
	if st := status("g1/b"); st != store.TaskPublished {
		t.Errorf("g1/b status = %s, want published", st)
	}
	if ev := lastEvent(t, s, "g1/b", "task-gate"); ev == nil {
		t.Error("b was not re-gated")
	}

	// 3. Squash merge for b: landed by patch.
	mainTip = forgeMerge(t, bare, func(f string) {
		gitC(t, f, "merge", "-q", "--squash", "origin/review/g1/b")
		gitC(t, f, "commit", "-q", "-m", "B (squashed)")
	})
	syncOK()
	wantLanded("g1/b", "patch", mainTip)

	// 4. Rebase merge for c (cherry-pick of its published commit): landed by patch.
	mainTip = forgeMerge(t, bare, func(f string) {
		gitC(t, f, "cherry-pick", "origin/review/g1/c")
	})
	syncOK()
	wantLanded("g1/c", "patch", mainTip)

	// 5. d merged with an extra edit but its message (and trailer) kept: landed by trailer.
	mainTip = forgeMerge(t, bare, func(f string) {
		msg := filepath.Join(t.TempDir(), "msg")
		if err := os.WriteFile(msg, []byte(gitC(t, f, "log", "-1", "--format=%B", "origin/review/g1/d")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f, "d.txt"), []byte("d\nextra edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitC(t, f, "add", "d.txt")
		gitC(t, f, "commit", "-q", "-F", msg)
	})
	if out := syncOK(); !strings.Contains(out, "Goals landed: g1") {
		t.Errorf("last sync output:\n%s", out)
	}
	wantLanded("g1/d", "trailer", mainTip)

	// 6. The goal is landed.
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalLanded {
		t.Errorf("goal status = %s, want landed", g.Status)
	}
	assertMain()
}

func TestE2EPushReviewRefusesNonReviewBranch(t *testing.T) {
	root, bare := e2eRepo(t)
	before := remoteRefs(t, bare)
	head := gitC(t, root, "rev-parse", "HEAD")
	for _, branch := range []string{"main", "feature/x", "review/", "refs/heads/main"} {
		if err := gitx.PushReview(context.Background(), root, "origin", head, branch, ""); err == nil {
			t.Errorf("PushReview to %q succeeded", branch)
		}
	}
	after := remoteRefs(t, bare)
	if len(after) != len(before) {
		t.Errorf("remote refs changed: %v -> %v", before, after)
	}
	for name, sha := range before {
		if after[name] != sha {
			t.Errorf("remote ref %s changed: %s -> %s", name, sha, after[name])
		}
	}
}
