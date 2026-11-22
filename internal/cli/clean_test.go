package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

func branchRow(t *testing.T, root, id string) store.Branch {
	t.Helper()
	b, err := openStore(t, root).GetBranch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCleanRemovesLandedWorktreeAndBranchOnly(t *testing.T) {
	root, _ := chainPublished(t, true)
	ctx := context.Background()
	a, b := branchRow(t, root, "g2/a"), branchRow(t, root, "g2/b")
	if a.Worktree == "" || !gitx.BranchExists(ctx, root, a.Name) {
		t.Fatalf("setup: expected a worktree and branch for g2/a: %+v", a)
	}
	setStatus(t, root, "g2/a", store.TaskLanded)

	r := run(t, root, nil, "clean")
	if r.code != ExitOK {
		t.Fatalf("clean: %+v", r)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree of g2/a still exists (err=%v)", err)
	}
	if gitx.BranchExists(ctx, root, a.Name) {
		t.Errorf("branch %s still exists", a.Name)
	}
	got := branchRow(t, root, "g2/a")
	if got.Worktree != "" || got.Name != a.Name || got.PublishedSHA != a.PublishedSHA {
		t.Errorf("branch row after clean = %+v", got)
	}
	lastEvent(t, openStore(t, root), "g2/a", "task-cleaned")
	if !strings.Contains(r.stdout, "1 worktree(s), 1 branch(es), 0 remote") {
		t.Errorf("summary missing counts:\n%s", r.stdout)
	}

	// The published task is untouched.
	if _, err := os.Stat(b.Worktree); err != nil {
		t.Errorf("worktree of published g2/b was removed: %v", err)
	}
	if !gitx.BranchExists(ctx, root, b.Name) || branchRow(t, root, "g2/b").Worktree != b.Worktree {
		t.Errorf("published g2/b was touched")
	}

	// Verified and blocked tasks are untouched too.
	for _, st := range []string{store.TaskVerified, store.TaskBlocked} {
		setStatus(t, root, "g2/b", st)
		if r := run(t, root, nil, "clean"); r.code != ExitOK {
			t.Fatalf("clean: %+v", r)
		}
		if _, err := os.Stat(b.Worktree); err != nil || !gitx.BranchExists(ctx, root, b.Name) {
			t.Errorf("%s task was cleaned", st)
		}
	}
}

func TestCleanTwiceIsHarmless(t *testing.T) {
	root, _ := chainPublished(t, true)
	setStatus(t, root, "g2/a", store.TaskLanded)
	if r := run(t, root, nil, "clean"); r.code != ExitOK {
		t.Fatalf("clean: %+v", r)
	}
	evs, _ := openStore(t, root).TaskEvents(context.Background(), "g2/a")
	r := run(t, root, nil, "clean")
	if r.code != ExitOK || !strings.Contains(r.stdout, "0 worktree(s), 0 branch(es)") {
		t.Fatalf("second clean: %+v", r)
	}
	after, _ := openStore(t, root).TaskEvents(context.Background(), "g2/a")
	if len(after) != len(evs) {
		t.Errorf("second clean recorded %d new event(s)", len(after)-len(evs))
	}
}

func TestCleanDryRunChangesNothing(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	a := branchRow(t, root, "g2/a")
	setStatus(t, root, "g2/a", store.TaskLanded)
	refs := remoteRefs(t, bare)

	r := run(t, root, nil, "clean", "--remote", "--dry-run")
	if r.code != ExitOK {
		t.Fatalf("clean --dry-run: %+v", r)
	}
	for _, want := range []string{"would remove worktree " + a.Worktree, "would delete branch " + a.Name, "would delete origin/review/g2/a", "Would clean"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	if _, err := os.Stat(a.Worktree); err != nil {
		t.Errorf("dry run removed the worktree: %v", err)
	}
	if !gitx.BranchExists(ctx, root, a.Name) || branchRow(t, root, "g2/a").Worktree != a.Worktree {
		t.Errorf("dry run changed the branch or its row")
	}
	for ref, sha := range refs {
		if remoteRefs(t, bare)[ref] != sha {
			t.Errorf("dry run changed remote %s", ref)
		}
	}
	evs, _ := openStore(t, root).TaskEvents(ctx, "g2/a")
	for _, e := range evs {
		if e.Kind == "task-cleaned" {
			t.Errorf("dry run recorded task-cleaned")
		}
	}
}

func TestCleanRemoteDeletesUnmovedSkipsGoneAndLeavesMoved(t *testing.T) {
	root, bare := chainPublished(t, true)
	a, b := branchRow(t, root, "g2/a"), branchRow(t, root, "g2/b")
	setStatus(t, root, "g2/a", store.TaskLanded)
	setStatus(t, root, "g2/b", store.TaskLanded)

	// g2/b's review branch moved on the remote; g2/a's is untouched.
	f := forge(t, bare)
	forgeCommit(t, f, "extra.txt", "x\n", "forge edit")
	gitC(t, f, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+b.PublishedRef)
	moved := remoteRefs(t, bare)["refs/heads/"+b.PublishedRef]

	r := run(t, root, nil, "clean", "--remote")
	if r.code != ExitOK {
		t.Fatalf("clean --remote: %+v", r)
	}
	refs := remoteRefs(t, bare)
	if _, ok := refs["refs/heads/"+a.PublishedRef]; ok {
		t.Errorf("review branch of g2/a still on the remote")
	}
	if refs["refs/heads/"+b.PublishedRef] != moved {
		t.Errorf("moved review branch of g2/b was not left alone")
	}
	if refs["refs/heads/main"] == "" {
		t.Errorf("main vanished from the remote")
	}
	if !strings.Contains(r.stdout, "1 remote branch(es)") || !strings.Contains(r.stdout, "g2/b: "+b.PublishedRef+" moved") {
		t.Errorf("summary wrong:\n%s", r.stdout)
	}

	// Again: g2/a's branch is already gone; that is skipped, not an error.
	r = run(t, root, nil, "clean", "--remote")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/a: "+a.PublishedRef+" already deleted") {
		t.Errorf("second clean --remote: %+v", r)
	}
}
