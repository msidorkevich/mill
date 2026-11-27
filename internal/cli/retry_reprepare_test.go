package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/store"
)

// retryScript runs g2/a once and g2/b twice (its first run and the retry).
const retryScript = `{"tasks": {
  "g2/a": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"a.txt": "a\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g2/b": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"b.txt": "b\n"}},
                           {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"b.txt": "b\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}},
                        {"output": {"verdict": "pass", "summary": "ok", "findings": []}}]}
}}`

// blockedDependentRepo runs the chain plan as g2 to verified, blocks g2/b and commits file
// with body on g2/a's branch. It returns the repo root and g2/a's new head.
func blockedDependentRepo(t *testing.T, file, body string) (root, aTip string) {
	t.Helper()
	root = goalRepo(t, retryScript, "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, chainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	setStatus(t, root, "g2/b", store.TaskBlocked)
	a, err := openStore(t, root).GetBranch(context.Background(), "g2/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Worktree, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, a.Worktree, "add", file)
	gitC(t, a.Worktree, "commit", "-q", "-m", "extra")
	return root, gitC(t, a.Worktree, "rev-parse", "HEAD")
}

func TestRetryBlockedReprepareRebasesDependentOntoMovedDependency(t *testing.T) {
	t.Parallel()
	root, aTip := blockedDependentRepo(t, "extra.txt", "e\n")

	r := groupBRun(t, root, "run", "g2", "--retry-blocked")
	if r.code != ExitOK {
		t.Fatalf("retry run: %+v", r)
	}
	if !strings.Contains(r.stdout, "g2/b: dependencies changed since preparation; rebased") {
		t.Errorf("stdout lacks the rebase line:\n%s", r.stdout)
	}
	b, err := openStore(t, root).GetBranch(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	// gitC fails the test unless the dependency's new head is an ancestor of b's branch.
	gitC(t, root, "merge-base", "--is-ancestor", aTip, b.Name)
	if b.StartSHA != aTip {
		t.Errorf("g2/b starts at %s, want a's new head %s", b.StartSHA, aTip)
	}
	if _, err := os.Stat(filepath.Join(b.Worktree, "extra.txt")); err != nil {
		t.Errorf("g2/b's worktree lacks the dependency's new commit: %v", err)
	}
	if got, _ := statusOf(t, openStore(t, root), "g2/b"); got != store.TaskVerified {
		t.Errorf("g2/b = %s, want verified", got)
	}
}

func TestRetryBlockedReprepareConflictLeavesTaskBlocked(t *testing.T) {
	t.Parallel()
	// a's new commit writes b.txt differently from b's own commit: replaying b conflicts.
	root, _ := blockedDependentRepo(t, "b.txt", "from a\n")
	s := openStore(t, root)
	before, err := s.GetBranch(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}

	r := groupBRun(t, root, "run", "g2", "--retry-blocked")
	if r.code != ExitNotVerified {
		t.Fatalf("retry run: %+v", r)
	}
	if strings.Contains(r.stdout, "g2/b: started") {
		t.Errorf("g2/b started on its stale starting point:\n%s", r.stdout)
	}
	tk, err := s.GetTask(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "conflicts") || !strings.Contains(tk.Reason, "mill task rebase g2/b") {
		t.Errorf("g2/b = %s (%s), want blocked with a conflict reason naming `mill task rebase g2/b`", tk.Status, tk.Reason)
	}
	after, err := s.GetBranch(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadSHA != before.HeadSHA || gitC(t, root, "rev-parse", after.Name) != before.HeadSHA {
		t.Errorf("g2/b's branch moved: %s -> %s", before.HeadSHA, after.HeadSHA)
	}
}
