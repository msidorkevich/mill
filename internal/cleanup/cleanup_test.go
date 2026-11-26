package cleanup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
// under wtRoot/goal/x.
func landedTask(t *testing.T) (Options, store.Branch, string) {
	t.Helper()
	return landedTaskAt(t, filepath.Join("goal", "x"))
}

// landedTaskAt makes a repo with one landed task "x" on branch mill/t/x whose worktree is
// wtRoot/rel, where wtRoot is returned.
func landedTaskAt(t *testing.T, rel string) (Options, store.Branch, string) {
	t.Helper()
	return landedTaskWithID(t, "x", rel)
}

// landedTaskWithID is landedTaskAt for a task with the given id.
func landedTaskWithID(t *testing.T, id, rel string) (Options, store.Branch, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wtRoot := t.TempDir()
	wt := filepath.Join(wtRoot, rel)
	git(t, root, "worktree", "add", "-q", "-b", "mill/t/x", wt, "main")
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "mill.db"), store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateTask(ctx, taskspec.Spec{ID: id, Title: "x", Kind: "implement", Spec: "s", Acceptance: []string{"ok"}, Size: "S"}); err != nil {
		t.Fatal(err)
	}
	br := store.Branch{TaskID: id, Name: "mill/t/x", Base: "main", Worktree: wt}
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, id, store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	return Options{Store: s, Root: root, Remote: "origin", Claimer: store.NewClaimer("clean")}, br, wtRoot
}

func TestCleanupKeepsDirtyWorktreeAndBranch(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestCleanupLeavesBusyTaskAloneAndReleasesItsClaims(t *testing.T) {
	t.Parallel()
	o, br, _ := landedTask(t)
	ctx := context.Background()
	if _, err := o.Store.StartAttempt(ctx, "x", 1, "implementer"); err != nil {
		t.Fatal(err)
	}
	r, err := Task(ctx, o, "x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Busy != "attempt 1 (implementer) running" || r.Cleaned() || len(r.Failures) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(br.Worktree); err != nil || !gitx.BranchExists(ctx, o.Root, br.Name) {
		t.Errorf("worktree or branch of a busy task removed (err=%v)", err)
	}
	claims, err := o.Store.ClaimedTasks(ctx)
	if err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

func TestCleanupClaimIsReleasedAfterCleaning(t *testing.T) {
	t.Parallel()
	o, br, _ := landedTask(t)
	ctx := context.Background()
	r, err := Task(ctx, o, "x")
	if err != nil || r.Worktree != br.Worktree {
		t.Fatalf("result = %+v, %v", r, err)
	}
	claims, err := o.Store.ClaimedTasks(ctx)
	if err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

// makeAttemptTmp writes a file under wtRoot/.tmp/<id>/a1-implementer and returns the task's
// tmp directory.
func makeAttemptTmp(t *testing.T, wtRoot, id string) string {
	t.Helper()
	dir := filepath.Join(wtRoot, ".tmp", id, "a1-implementer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wtRoot, ".tmp", id)
}

func TestCleanupRemovesLandedTaskTmpDirAndLeavesOtherTasks(t *testing.T) {
	t.Parallel()
	o, _, wtRoot := landedTaskWithID(t, "g/x", filepath.Join("g", "x"))
	ctx := context.Background()
	tmp := makeAttemptTmp(t, wtRoot, "g/x")
	other := makeAttemptTmp(t, wtRoot, "g/y")
	var out bytes.Buffer
	o.Out = &out
	o.KeepDirty = true
	r, err := Task(ctx, o, "g/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failures) != 0 || r.Kept != "" {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("task tmp dir still exists (err=%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other task tmp dir removed: %v", err)
	}
	if want := "g/x  removed tmp dir " + tmp + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want line %q", out.String(), want)
	}
}

func TestCleanupDryRunReportsTaskTmpDirAndKeepsIt(t *testing.T) {
	t.Parallel()
	o, _, wtRoot := landedTaskWithID(t, "g/x", filepath.Join("g", "x"))
	ctx := context.Background()
	tmp := makeAttemptTmp(t, wtRoot, "g/x")
	other := makeAttemptTmp(t, wtRoot, "g/y")
	var out bytes.Buffer
	o.Out = &out
	o.DryRun = true
	if _, err := Task(ctx, o, "g/x"); err != nil {
		t.Fatal(err)
	}
	if want := "g/x  would remove tmp dir " + tmp + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want line %q", out.String(), want)
	}
	if _, err := os.Stat(filepath.Join(tmp, "a1-implementer", "prompt.txt")); err != nil {
		t.Errorf("dry run removed the tmp dir: %v", err)
	}
	if _, err := os.Stat(other); err != nil || strings.Contains(out.String(), other) {
		t.Errorf("other task tmp dir removed or reported (err=%v, output=%q)", err, out.String())
	}
}

func TestCleanupKeepsTaskTmpDirOfDirtyWorktree(t *testing.T) {
	t.Parallel()
	o, br, wtRoot := landedTaskWithID(t, "g/x", filepath.Join("g", "x"))
	ctx := context.Background()
	tmp := makeAttemptTmp(t, wtRoot, "g/x")
	if err := os.WriteFile(filepath.Join(br.Worktree, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.KeepDirty = true
	r, err := Task(ctx, o, "g/x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kept == "" || len(r.Failures) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("tmp dir of a kept task removed: %v", err)
	}
}

func TestCleanupReportsTaskTmpDirRemovalFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	o, _, wtRoot := landedTaskWithID(t, "g/x", filepath.Join("g", "x"))
	ctx := context.Background()
	makeAttemptTmp(t, wtRoot, "g/x")
	// Removing the attempt directory needs write access to .tmp/g/x, which is now read-only.
	ro := filepath.Join(wtRoot, ".tmp", "g", "x")
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	r, err := Task(ctx, o, "g/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failures) != 1 || r.Failures[0].Err == nil || r.Failures[0].Step != "remove tmp dir "+ro {
		t.Fatalf("failures = %+v", r.Failures)
	}
}

func TestCleanupRemovesTaskTmpDirWhenWorktreeIsAlreadyGone(t *testing.T) {
	t.Parallel()
	o, br, wtRoot := landedTaskWithID(t, "g/x", filepath.Join("g", "x"))
	ctx := context.Background()
	tmp := makeAttemptTmp(t, wtRoot, "g/x")
	if err := os.RemoveAll(br.Worktree); err != nil {
		t.Fatal(err)
	}
	r, err := Task(ctx, o, "g/x")
	if err != nil || len(r.Failures) != 0 {
		t.Fatalf("result = %+v, %v", r, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("task tmp dir still exists (err=%v)", err)
	}
}

func TestCleanupReportsUnknownTmpDirWhenWorktreePathDoesNotEndInTaskID(t *testing.T) {
	t.Parallel()
	o, br, wtRoot := landedTaskWithID(t, "g/x", "elsewhere")
	ctx := context.Background()
	tmp := makeAttemptTmp(t, wtRoot, "g/x")
	r, err := Task(ctx, o, "g/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failures) != 1 || r.Failures[0].Step != "locate tmp dir" {
		t.Fatalf("failures = %+v", r.Failures)
	}
	if r.Worktree != br.Worktree || r.Branch != br.Name {
		t.Errorf("rest of the cleanup not done: %+v", r)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("tmp dir removed on a guess: %v", err)
	}
}
