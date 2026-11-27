package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// wtExclGitDir returns the absolute git dir of the worktree at dir, symlinks resolved.
func wtExclGitDir(t *testing.T, dir string) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(mustRun(t, context.Background(), dir, "rev-parse", "--absolute-git-dir"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// wtExclLock takes LockWorktree with 1ms flock polls and fails the test on error.
func wtExclLock(t *testing.T, ctx context.Context, dir string) (context.Context, func()) {
	t.Helper()
	lctx, unlock, err := lockWorktreePoll(ctx, dir, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return lctx, unlock
}

// wtExclTryLock tries LockWorktree for 50ms and reports whether it got the lock (released
// again at once).
func wtExclTryLock(t *testing.T, dir string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, unlock, err := lockWorktreePoll(ctx, dir, time.Millisecond)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock failed for another reason than the wait ending: %v", err)
		}
		return false
	}
	unlock()
	return true
}

// wtExclIndexLock writes <git dir>/index.lock of dir with modification time mtime.
func wtExclIndexLock(t *testing.T, dir string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(wtExclGitDir(t, dir), "index.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// wtExclLog collects log lines.
type wtExclLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *wtExclLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *wtExclLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func wtExclExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestLockWorktreeExcludesSecondGoroutineUntilUnlock(t *testing.T) {
	t.Parallel()
	r := repo(t)
	_, unlock := wtExclLock(t, context.Background(), r)

	got := make(chan error, 1)
	release := make(chan struct{})
	go func() {
		_, unlock2, err := lockWorktreePoll(context.Background(), r, time.Millisecond)
		got <- err
		if err == nil {
			<-release
			unlock2()
		}
	}()
	select {
	case err := <-got:
		t.Fatalf("second goroutine got the lock while it was held (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second goroutine did not get the lock after unlock")
	}
	close(release)
}

func TestLockWorktreeExcludesOtherProcesses(t *testing.T) {
	t.Parallel()
	r := repo(t)
	lockPath := filepath.Join(wtExclGitDir(t, r), worktreeExclusiveFile)
	// A flock taken through another open file description conflicts exactly as one taken by
	// another process does.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, unlock, err := lockWorktreePoll(ctx, r, time.Millisecond); err == nil {
		unlock()
		t.Fatal("lock taken while another descriptor holds the flock")
	} else if !strings.Contains(err.Error(), lockPath) {
		t.Errorf("error %q does not name the lock file %s", err, lockPath)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !wtExclTryLock(t, r) {
		t.Fatal("lock not taken after the other descriptor released the flock")
	}
}

func TestLockWorktreeLeavesOtherWorktreesUnaffected(t *testing.T) {
	t.Parallel()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(context.Background(), r, wt, "wt/other", "main"); err != nil {
		t.Fatal(err)
	}
	_, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()
	if !wtExclTryLock(t, wt) {
		t.Fatal("locking the main worktree blocked another worktree of the same repository")
	}
	if wtExclTryLock(t, r) {
		t.Fatal("main worktree locked twice")
	}
}

func TestLockWorktreeNestedWithReturnedContextDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	r := repo(t)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()

	done := make(chan error, 1)
	go func() {
		nctx, nunlock, err := LockWorktree(ctx, r)
		if err == nil {
			if nctx != ctx {
				err = errors.New("nested LockWorktree returned a different context")
			}
			nunlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested LockWorktree with the returned context deadlocked")
	}
	// The nested unlock was a no-op: the outer lock still excludes others.
	if wtExclTryLock(t, r) {
		t.Fatal("nested unlock released the outer lock")
	}
	unlock()
	if !wtExclTryLock(t, r) {
		t.Fatal("lock not free after the outer unlock")
	}
}

func TestLockWorktreeWaitGivesUpWhenContextEnds(t *testing.T) {
	t.Parallel()
	r := repo(t)
	_, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, unlock2, err := lockWorktreePoll(ctx, r, time.Millisecond)
		if err == nil {
			unlock2()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lock taken while held")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error %v does not report the cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock wait did not end with its context")
	}
}

func TestClearStaleIndexLockRemovesOldOneAndLogs(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()
	var log wtExclLog
	now := func() time.Time { return mtime.Add(10 * time.Minute) }
	removed, err := clearStaleIndexLock(ctx, r, time.Minute, now, log.logf, func([]string) (bool, error) { return false, nil })
	if err != nil || !removed {
		t.Fatalf("ClearStaleIndexLock = %v, %v; want removed", removed, err)
	}
	if wtExclExists(t, path) {
		t.Fatal("old index.lock still exists")
	}
	want := fmt.Sprintf("removed stale %s (age 10m0s, no git process in %s)", path, r)
	if log.String() != want {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
}

func TestClearStaleIndexLockWithRealProcessCheckRemovesOldOne(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()
	var log wtExclLog
	removed, err := ClearStaleIndexLock(ctx, r, time.Minute, func() time.Time { return mtime.Add(time.Hour) }, log.logf)
	if err != nil {
		// The machine may not let us list process directories; then nothing is removed.
		if !strings.Contains(err.Error(), "cannot tell whether a git process holds "+path) || !wtExclExists(t, path) {
			t.Fatalf("undecided check: err %v, index.lock exists %v", err, wtExclExists(t, path))
		}
		t.Skipf("process check cannot decide here: %v", err)
	}
	if !removed || wtExclExists(t, path) {
		t.Fatalf("removed = %v, index.lock exists %v; want it removed", removed, wtExclExists(t, path))
	}
	if !strings.Contains(log.String(), "removed stale "+path) {
		t.Errorf("log %q does not report the removal", log.String())
	}
}

func TestClearStaleIndexLockLeavesFreshOne(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()
	var log wtExclLog
	now := func() time.Time { return mtime.Add(30 * time.Second) }
	removed, err := clearStaleIndexLock(ctx, r, time.Minute, now, log.logf, func([]string) (bool, error) { return false, nil })
	if err != nil || removed {
		t.Fatalf("ClearStaleIndexLock = %v, %v; want left alone", removed, err)
	}
	if !wtExclExists(t, path) {
		t.Fatal("fresh index.lock was removed")
	}
	if log.String() != "" {
		t.Errorf("logged %q for a fresh index.lock", log.String())
	}
}

func TestClearStaleIndexLockRefusesWithoutLockHeld(t *testing.T) {
	t.Parallel()
	r := repo(t)
	other := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	// Holding the lock of another worktree does not count.
	ctx, unlock := wtExclLock(t, context.Background(), other)
	defer unlock()
	now := func() time.Time { return mtime.Add(time.Hour) }
	for name, c := range map[string]context.Context{"no lock": context.Background(), "other worktree locked": ctx} {
		var log wtExclLog
		removed, err := ClearStaleIndexLock(c, r, time.Minute, now, log.logf)
		if err == nil || removed {
			t.Errorf("%s: ClearStaleIndexLock = %v, %v; want an error", name, removed, err)
		} else if !strings.Contains(err.Error(), "not locked") {
			t.Errorf("%s: error %q does not say the worktree is not locked", name, err)
		}
		if !wtExclExists(t, path) {
			t.Fatalf("%s: index.lock removed without the lock held", name)
		}
	}
}

func TestClearStaleIndexLockKeepsFileWhenGitProcessRuns(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()

	// A git process waiting on stdin with its working directory in the worktree.
	cmd := exec.Command(Bin(), "cat-file", "--batch")
	cmd.Dir = r
	cmd.Env = cleanEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()

	var log wtExclLog
	removed, err := ClearStaleIndexLock(ctx, r, time.Minute, func() time.Time { return mtime.Add(time.Hour) }, log.logf)
	if removed {
		t.Fatal("index.lock removed while a git process runs in the worktree")
	}
	if !wtExclExists(t, path) {
		t.Fatal("index.lock gone while a git process runs in the worktree")
	}
	if err != nil && !strings.Contains(err.Error(), "cannot tell whether a git process holds "+path) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClearStaleIndexLockFailsWhenProcessCheckCannotDecide(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mtime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := wtExclIndexLock(t, r, mtime)
	ctx, unlock := wtExclLock(t, context.Background(), r)
	defer unlock()
	var log wtExclLog
	undecided := func([]string) (bool, error) { return false, errors.New("no process table") }
	removed, err := clearStaleIndexLock(ctx, r, time.Minute, func() time.Time { return mtime.Add(time.Hour) }, log.logf, undecided)
	if err == nil || removed {
		t.Fatalf("ClearStaleIndexLock = %v, %v; want an error", removed, err)
	}
	want := "cannot tell whether a git process holds " + path + "; remove it by hand if none runs"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if !wtExclExists(t, path) {
		t.Fatal("index.lock removed though the process check could not decide")
	}
	if log.String() != "" {
		t.Errorf("logged %q without removing", log.String())
	}
}
