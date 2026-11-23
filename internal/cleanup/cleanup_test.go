package cleanup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// landedTask makes a repo with one landed task "x" on branch mill/t/x with its own worktree
// under wtRoot/x.
func landedTask(t *testing.T) (Options, store.Branch, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wtRoot := t.TempDir()
	wt := filepath.Join(wtRoot, "goal", "x")
	git(t, root, "worktree", "add", "-q", "-b", "mill/t/x", wt, "main")
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "mill.db"), store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateTask(ctx, taskspec.Spec{ID: "x", Title: "x", Kind: "implement", Spec: "s", Acceptance: []string{"ok"}, Size: "S"}); err != nil {
		t.Fatal(err)
	}
	br := store.Branch{TaskID: "x", Name: "mill/t/x", Base: "main", Worktree: wt}
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	return Options{Store: s, Root: root, Remote: "origin"}, br, wtRoot
}

func TestCleanupKeepsDirtyWorktreeAndBranch(t *testing.T) {
	o, br, _ := landedTask(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(br.Worktree, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.KeepDirty = true
	r, err := Task(ctx, o, "x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kept == "" || r.Cleaned() || len(r.Failures) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(br.Worktree); err != nil || !gitx.BranchExists(ctx, o.Root, br.Name) {
		t.Errorf("worktree or branch removed (err=%v)", err)
	}
	if got, _ := o.Store.GetBranch(ctx, "x"); got.Worktree != br.Worktree {
		t.Errorf("branch row worktree = %q", got.Worktree)
	}
}

func TestCleanupRemovesWorktreeAndBranchThenPrunesEmptyDirs(t *testing.T) {
	o, br, wtRoot := landedTask(t)
	ctx := context.Background()
	o.KeepDirty = true
	r, err := Task(ctx, o, "x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Worktree != br.Worktree || r.Branch != br.Name || r.RemoteBranch != "" || len(r.Failures) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if gitx.BranchExists(ctx, o.Root, br.Name) {
		t.Error("branch still exists")
	}
	evs, err := o.Store.TaskEvents(ctx, "x")
	if err != nil || len(evs) == 0 || evs[len(evs)-1].Kind != "task-cleaned" {
		t.Errorf("events = %+v, %v", evs, err)
	}
	n, err := PruneEmptyDirs(nil, wtRoot, false)
	if err != nil || n != 1 {
		t.Errorf("pruned %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(wtRoot, "goal")); !os.IsNotExist(err) {
		t.Errorf("empty goal directory left (err=%v)", err)
	}
	if n, err := PruneEmptyDirs(nil, filepath.Join(wtRoot, "missing"), false); err != nil || n != 0 {
		t.Errorf("missing root: %d, %v", n, err)
	}
}
