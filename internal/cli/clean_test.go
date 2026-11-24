package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestCleanRemovesEmptyDirsUnderWorktreeRoot(t *testing.T) {
	t.Parallel()
	root, _ := chainPublished(t, true)
	setStatus(t, root, "g2/a", store.TaskLanded)
	a := branchRow(t, root, "g2/a")
	wtRoot := filepath.Dir(filepath.Dir(a.Worktree))
	mk := func(p string) string {
		p = filepath.Join(wtRoot, p)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("g1")
	mk(".base/g1-x/deep")
	keepDir := mk("keep/a/b")
	if err := os.WriteFile(filepath.Join(keepDir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	linkDir := mk("linked")
	if err := os.Symlink(outside, filepath.Join(linkDir, "l")); err != nil {
		t.Fatal(err)
	}
	emptyTarget := filepath.Join(outside, "empty")
	if err := os.Mkdir(emptyTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	r := run(t, root, nil, "clean", "--dry-run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "would remove empty directory "+filepath.Join(wtRoot, "g1")) {
		t.Fatalf("dry run: %+v", r)
	}
	for _, p := range []string{"g1", ".base/g1-x/deep"} {
		if _, err := os.Stat(filepath.Join(wtRoot, p)); err != nil {
			t.Errorf("dry run removed %s: %v", p, err)
		}
	}

	r = run(t, root, nil, "clean")
	if r.code != ExitOK {
		t.Fatalf("clean: %+v", r)
	}
	for _, p := range []string{"g1", ".base"} {
		if _, err := os.Stat(filepath.Join(wtRoot, p)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
	for _, p := range []string{"", "keep/a/b/f", "linked/l"} {
		if _, err := os.Lstat(filepath.Join(wtRoot, p)); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Stat(emptyTarget); err != nil {
		t.Errorf("symlink target was touched: %v", err)
	}
	if !strings.Contains(r.stdout, ", 4 empty director(ies)") {
		t.Errorf("summary lacks empty directory count:\n%s", r.stdout)
	}
	if r := run(t, root, nil, "clean"); r.code != ExitOK || !strings.Contains(r.stdout, ", 0 empty director(ies)") {
		t.Errorf("second clean: %+v", r)
	}
}

func TestCleanLeavesEmptyDirsInsideLiveWorktrees(t *testing.T) {
	t.Parallel()
	root, _ := chainPublished(t, true)
	setStatus(t, root, "g2/a", store.TaskLanded)
	a := branchRow(t, root, "g2/a")
	wtRoot := filepath.Dir(filepath.Dir(a.Worktree))
	live := filepath.Join(wtRoot, "g3", "live")
	outDir := filepath.Join(live, "out", "nested")
	for _, d := range []string{outDir, filepath.Join(wtRoot, "g1"), filepath.Join(wtRoot, ".base", "g1-x")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if r := run(t, root, nil, "clean"); r.code != ExitOK {
		t.Fatalf("clean: %+v", r)
	}
	if _, err := os.Stat(outDir); err != nil {
		t.Errorf("empty directory inside live worktree was removed: %v", err)
	}
	for _, p := range []string{"g1", ".base"} {
		if _, err := os.Stat(filepath.Join(wtRoot, p)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
}

func TestCleanMissingWorktreeRootIsNotAnError(t *testing.T) {
	t.Parallel()
	root, _ := chainPublished(t, true)
	a := branchRow(t, root, "g2/a")
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(a.Worktree))); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "clean"); r.code != ExitOK || !strings.Contains(r.stdout, "0 empty director(ies)") {
		t.Fatalf("clean: %+v", r)
	}
}

// setCleanOnLand sets clean_on_land in the project config written by goalRepo.
func setCleanOnLand(t *testing.T, root, mode string) {
	t.Helper()
	path := project.PathsAt(root).Config
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(data), "max_attempts = 1\n", "max_attempts = 1\nclean_on_land = \""+mode+"\"\n", 1)
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mergeReview merges review/<id> into main on the forge and pushes it.
func mergeReview(t *testing.T, f, id string) {
	t.Helper()
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/"+id, "origin/review/"+id)
	gitC(t, f, "push", "-q", "origin", "main")
}

func eventSeq(t *testing.T, s *store.Store, id, kind string) int64 {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == kind {
			return evs[i].Seq
		}
	}
	t.Fatalf("%s has no %s event", id, kind)
	return 0
}

func TestSyncCleansUpLandedTaskByDefaultAfterRestackingItsChild(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	a := branchRow(t, root, "g2/a")
	mergeReview(t, forge(t, bare), "g2/a")
	merge := remoteRefs(t, bare)["refs/heads/main"]

	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree of g2/a still exists (err=%v)", err)
	}
	if gitx.BranchExists(ctx, root, a.Name) {
		t.Errorf("branch %s still exists", a.Name)
	}
	if sha, ok := remoteRefs(t, bare)["refs/heads/review/g2/a"]; ok {
		t.Errorf("review/g2/a still on the remote at %s", sha)
	}
	s := openStore(t, root)
	if ev := lastEvent(t, s, "g2/a", "task-cleaned"); ev["branch"] != a.Name || ev["worktree"] != a.Worktree {
		t.Errorf("task-cleaned = %v", ev)
	}
	if got := branchRow(t, root, "g2/a"); got.Worktree != "" {
		t.Errorf("branch row keeps worktree %q", got.Worktree)
	}
	for _, want := range []string{"Cleaned up:", "g2/a  worktree " + a.Worktree + ", branch " + a.Name + ", origin/review/g2/a"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	// The child was restacked onto main before its parent's branch went away, correctly.
	if eventSeq(t, s, "g2/b", "task-restacked") > eventSeq(t, s, "g2/a", "task-cleaned") {
		t.Error("g2/a was cleaned up before g2/b was restacked")
	}
	child := branchRow(t, root, "g2/b")
	if child.Base != "main" || child.BaseSHA != merge || child.GateSHA != child.HeadSHA {
		t.Errorf("child branch = %+v", child)
	}
	if files := gitC(t, root, "diff", "--name-only", merge, child.HeadSHA); files != "b.txt" {
		t.Errorf("child's own changes = %q", files)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/b"]; got != child.PublishedSHA {
		t.Errorf("review/g2/b = %s, recorded %s", got, child.PublishedSHA)
	}
	if _, err := os.Stat(child.Worktree); err != nil {
		t.Errorf("child worktree: %v", err)
	}
}

func TestSyncLeavesMovedReviewBranchOnTheRemote(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	a := branchRow(t, root, "g2/a")
	f := forge(t, bare)
	mergeReview(t, f, "g2/a")
	// Someone pushes another commit to the review branch after the merge.
	gitC(t, f, "checkout", "-q", "-B", "extra", "origin/review/g2/a")
	forgeCommit(t, f, "extra.txt", "x\n", "extra")
	gitC(t, f, "push", "-q", "origin", "extra:review/g2/a")
	moved := gitC(t, f, "rev-parse", "HEAD")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/a"]; got != moved {
		t.Errorf("review/g2/a = %q, want it left at %s", got, moved)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree of g2/a still exists (err=%v)", err)
	}
	if !strings.Contains(r.stdout, "Not cleaned up") || !strings.Contains(r.stdout, "review/g2/a moved on origin") {
		t.Errorf("moved review branch not reported:\n%s", r.stdout)
	}
}

func TestSyncCleanOnLandLocalKeepsRemoteBranch(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	setCleanOnLand(t, root, "local")
	a := branchRow(t, root, "g2/a")
	mergeReview(t, forge(t, bare), "g2/a")

	r := run(t, root, nil, "sync", "--json")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	var doc struct {
		Schema  string `json:"schema"`
		Cleaned []struct {
			TaskID       string `json:"task_id"`
			Worktree     string `json:"worktree"`
			Branch       string `json:"branch"`
			RemoteBranch string `json:"remote_branch"`
		} `json:"cleaned"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("decode %q: %v", r.stdout, err)
	}
	if doc.Schema != "mill/sync@1" || len(doc.Cleaned) != 1 || doc.Cleaned[0].TaskID != "g2/a" ||
		doc.Cleaned[0].Worktree != a.Worktree || doc.Cleaned[0].Branch != a.Name || doc.Cleaned[0].RemoteBranch != "" {
		t.Errorf("json = %+v", doc)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree of g2/a still exists (err=%v)", err)
	}
	if gitx.BranchExists(context.Background(), root, a.Name) {
		t.Errorf("branch %s still exists", a.Name)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/a"]; got != a.PublishedSHA {
		t.Errorf("review/g2/a = %q, want it kept at %s", got, a.PublishedSHA)
	}
}

func TestSyncCleanOnLandOffCleansNothing(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	setCleanOnLand(t, root, "off")
	a := branchRow(t, root, "g2/a")
	mergeReview(t, forge(t, bare), "g2/a")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/a: landed") {
		t.Fatalf("sync: %+v", r)
	}
	if strings.Contains(r.stdout, "Cleaned up") {
		t.Errorf("sync cleaned up with clean_on_land = off:\n%s", r.stdout)
	}
	if _, err := os.Stat(a.Worktree); err != nil || !gitx.BranchExists(context.Background(), root, a.Name) {
		t.Errorf("g2/a worktree or branch removed (err=%v)", err)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/a"]; got != a.PublishedSHA {
		t.Errorf("review/g2/a = %q", got)
	}
	evs, _ := openStore(t, root).TaskEvents(context.Background(), "g2/a")
	for _, e := range evs {
		if e.Kind == "task-cleaned" {
			t.Error("task-cleaned recorded")
		}
	}
	// `mill clean` still cleans it up.
	if r := run(t, root, nil, "clean"); r.code != ExitOK || !strings.Contains(r.stdout, "1 worktree(s), 1 branch(es)") {
		t.Errorf("clean: %+v", r)
	}
}

func TestSyncKeepsDirtyWorktreeOfLandedTask(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	a := branchRow(t, root, "g2/a")
	if err := os.WriteFile(filepath.Join(a.Worktree, "notes.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeReview(t, forge(t, bare), "g2/a")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(a.Worktree, "notes.txt")); err != nil {
		t.Errorf("dirty worktree removed: %v", err)
	}
	if !gitx.BranchExists(context.Background(), root, a.Name) || branchRow(t, root, "g2/a").Worktree != a.Worktree {
		t.Errorf("branch of g2/a not kept")
	}
	for _, want := range []string{"Not cleaned up", "g2/a  worktree " + a.Worktree + " has uncommitted changes"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "Cleaned up:") {
		t.Errorf("dirty task listed as cleaned:\n%s", r.stdout)
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s", tk.Status)
	}
}

func TestCleanWorksWhileRunLockIsHeldAndSkipsBusyTask(t *testing.T) {
	t.Parallel()
	root, _ := chainPublished(t, true)
	ctx := context.Background()
	a, b := branchRow(t, root, "g2/a"), branchRow(t, root, "g2/b")
	setStatus(t, root, "g2/a", store.TaskLanded)
	setStatus(t, root, "g2/b", store.TaskLanded)
	w := startBusyAttempt(t, root, "g2/b")
	lock, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	r := run(t, root, nil, "clean")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Skipped:\n  g2/b: busy ("+w.doing+")") {
		t.Fatalf("clean: %+v", r)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) || gitx.BranchExists(ctx, root, a.Name) {
		t.Errorf("g2/a not cleaned (err=%v)", err)
	}
	if _, err := os.Stat(b.Worktree); err != nil || !gitx.BranchExists(ctx, root, b.Name) || branchRow(t, root, "g2/b").Worktree != b.Worktree {
		t.Errorf("busy g2/b was cleaned (err=%v)", err)
	}
	w.assertUntouched(t)
	assertNoClaims(t, root)
}
