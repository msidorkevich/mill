package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// worktreeLockFile is the name, inside the git common dir, of the file whose flock serializes
// worktree administration (git worktree add/remove/prune/move/repair) across processes.
const worktreeLockFile = "mill-worktrees.lock"

// worktreeLockPoll is how often a blocked flock is retried.
const worktreeLockPoll = 20 * time.Millisecond

// worktreeLocks serializes worktree administration of one repository within the process.
var worktreeLocks sync.Map // git common dir -> chan struct{} (1 slot; full = held)

// lockWorktreeAdmin takes the per-repository worktree administration lock of repo: an
// in-process mutex keyed by the git common dir, then an exclusive flock on
// <common dir>/mill-worktrees.lock. Concurrent `git worktree add|remove|prune` in one
// repository race on .git/worktrees and fail; this lock runs them one at a time. Both waits
// give up when ctx is done.
func lockWorktreeAdmin(ctx context.Context, repo string) (unlock func(), err error) {
	return lockWorktreeAdminPoll(ctx, repo, worktreeLockPoll)
}

// lockWorktreeAdminPoll is lockWorktreeAdmin with the flock retry interval given.
func lockWorktreeAdminPoll(ctx context.Context, repo string, poll time.Duration) (unlock func(), err error) {
	dir, err := CommonDir(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("lock worktrees of %s: find the git common dir: %w", repo, err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("lock worktrees of %s: %w", repo, err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	path := filepath.Join(dir, worktreeLockFile)

	v, _ := worktreeLocks.LoadOrStore(dir, make(chan struct{}, 1))
	mu := v.(chan struct{})
	select {
	case mu <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("lock worktrees of %s: %w; another worktree change in this mill is still holding %s", dir, ctx.Err(), path)
	}
	f, err := flockFile(ctx, path, poll)
	if err != nil {
		<-mu
		return nil, fmt.Errorf("lock worktrees of %s: %w; another mill may be stuck holding %s", dir, err, path)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the descriptor releases the flock too; unlock first so the release
			// does not depend on close succeeding.
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			<-mu
		})
	}, nil
}

// flockFile opens path (creating it with mode 0644 if missing) and takes an exclusive flock
// on it, retrying every poll until ctx is done. The returned file holds the lock; closing it
// releases the lock. The file is never deleted.
func flockFile(ctx context.Context, path string, poll time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	var t *time.Timer
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if t != nil {
				t.Stop()
			}
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		if t == nil {
			t = time.NewTimer(poll)
		} else {
			t.Reset(poll)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("flock %s: still held by another process when the wait ended: %w", path, ctx.Err())
		}
	}
}
