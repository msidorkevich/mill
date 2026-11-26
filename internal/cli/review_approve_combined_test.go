package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
)

// siblingsPlan has three independent tasks: a and b each add func Helper to package p, c adds
// an unrelated file.
const siblingsPlan = `{
  "goal": "Siblings",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "add Helper in h1.go", "acceptance": ["h1.go exists"],
     "checks": ["test -f h1.go"], "owns": ["h1.go"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "add Helper in h2.go", "acceptance": ["h2.go exists"],
     "checks": ["test -f h2.go"], "owns": ["h2.go"], "depends_on": [], "size": "S"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "add Other in other.go", "acceptance": ["other.go exists"],
     "checks": ["test -f other.go"], "owns": ["other.go"], "depends_on": [], "size": "S"}
  ]
}`

const siblingsScript = `{"tasks": {
  "g1/a": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"h1.go": "package p\n\nfunc Helper() {}\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g1/b": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"h2.go": "package p\n\nfunc Helper() {}\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g1/c": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"other.go": "package p\n\nfunc Other() {}\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]}
}}`

// siblingsRepo is a tiny Go module with a bare remote whose goal g1 has three verified sibling
// tasks; the gate is `go build ./...`. Each branch builds alone; a and b together do not.
func siblingsRepo(t *testing.T) (root, bare string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root = gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "go build ./..."
max_attempts = 1
max_parallel = 3
worktree_root = "` + t.TempDir() + `"
env_allow = ["GOCACHE", "GOPATH", "GOMODCACHE", "GOROOT", "GOTOOLCHAIN"]
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	files := map[string]string{
		project.PathsAt(root).Config:       cfg,
		filepath.Join(root, "script.json"): siblingsScript,
		filepath.Join(root, "go.mod"):      "module example.com/p\n\ngo 1.21\n",
		filepath.Join(root, "p.go"):        "package p\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "plan", "--file", writePlan(t, siblingsPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("plan approve: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	return root, addRemote(t, root)
}

// shortApproveGateSlotPoll makes approvals poll for a gate slot every millisecond for the
// test. Tests calling it must not run in parallel.
func shortApproveGateSlotPoll(t *testing.T) {
	t.Helper()
	old := approveGateSlotPoll
	approveGateSlotPoll = time.Millisecond
	t.Cleanup(func() { approveGateSlotPoll = old })
}

// Not parallel: it shortens approveGateSlotPoll for the package.
func TestApproveGoalHoldsBackClashingSiblings(t *testing.T) {
	shortApproveGateSlotPoll(t)
	root, bare := siblingsRepo(t)

	r := groupBRun(t, root, "review", "approve", "--goal", "g1")
	if r.code == ExitOK {
		t.Fatalf("approve --goal exited 0 though a pair was held back: %+v", r)
	}
	want := "combined check: g1/b conflicts with g1/a: "
	if !strings.Contains(r.stderr, want) || !strings.Contains(r.stderr, "not publishing them. Fix one and run `mill review approve` again") {
		t.Errorf("stderr does not name the held-back pair (want %q):\n%s", want, r.stderr)
	}
	if !strings.Contains(r.stdout, "Published g1/c ") {
		t.Errorf("g1/c was not published:\n%s", r.stdout)
	}
	refs := remoteRefs(t, bare)
	if refs["refs/heads/review/g1/c"] == "" {
		t.Errorf("review/g1/c not on the remote: %v", refs)
	}
	for _, id := range []string{"a", "b"} {
		if refs["refs/heads/review/g1/"+id] != "" {
			t.Errorf("clashing task g1/%s was published: %v", id, refs)
		}
		if strings.Contains(r.stdout, "Published g1/"+id+" ") {
			t.Errorf("output reports g1/%s published:\n%s", id, r.stdout)
		}
	}
	if _, ok, err := openStore(t, root).LatestEvent(context.Background(), "combined-checked"); err != nil || !ok {
		t.Errorf("no combined-checked event recorded (err %v)", err)
	}
	if claims, err := openStore(t, root).ClaimedTasks(context.Background()); err != nil || len(claims) != 0 {
		t.Errorf("claims left after approve: %v (err %v)", claims, err)
	}
}

// Not parallel: it shortens approveGateSlotPoll for the package.
func TestApproveGoalNoCombinedPublishesAll(t *testing.T) {
	shortApproveGateSlotPoll(t)
	root, bare := siblingsRepo(t)

	r := groupBRun(t, root, "review", "approve", "--goal", "g1", "--no-combined")
	if r.code != ExitOK {
		t.Fatalf("approve --goal --no-combined: %+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, "combined check") {
		t.Errorf("a combined check ran:\n%s\n%s", r.stdout, r.stderr)
	}
	refs := remoteRefs(t, bare)
	for _, id := range []string{"a", "b", "c"} {
		if refs["refs/heads/review/g1/"+id] == "" {
			t.Errorf("review/g1/%s not on the remote: %v", id, refs)
		}
	}
	if _, ok, err := openStore(t, root).LatestEvent(context.Background(), "combined-checked"); err != nil || ok {
		t.Errorf("combined-checked event recorded with --no-combined (ok %v, err %v)", ok, err)
	}
}

// Not parallel: it shortens approveGateSlotPoll for the package.
func TestApproveGoalStopsWhenCombinedCannotDecide(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(t *testing.T, root, bare string)
	}{
		{"remote has no base branch", func(t *testing.T, root, bare string) {
			gitC(t, bare, "update-ref", "-d", "refs/heads/main")
		}},
		{"fetch fails", func(t *testing.T, root, bare string) {
			gitC(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortApproveGateSlotPoll(t)
			root, bare := siblingsRepo(t)
			tc.breakIt(t, root, bare)

			r := groupBRun(t, root, "review", "approve", "--goal", "g1")
			if r.code == ExitOK {
				t.Fatalf("approve --goal exited 0 though the combined check could not run: %+v", r)
			}
			if !strings.Contains(r.stderr, "--no-combined") {
				t.Errorf("stderr does not suggest --no-combined:\n%s", r.stderr)
			}
			for name := range remoteRefs(t, bare) {
				if strings.HasPrefix(name, "refs/heads/review/") {
					t.Errorf("%s was published though the combined check could not run", name)
				}
			}
			if strings.Contains(r.stdout, "Published ") {
				t.Errorf("output reports a publish:\n%s", r.stdout)
			}
		})
	}
}
