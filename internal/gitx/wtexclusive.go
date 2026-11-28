package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// worktreeExclusiveFile is the name, inside a worktree's git dir, of the file whose flock
// holds that worktree exclusively across processes.
const worktreeExclusiveFile = "mill-worktree.lock"

// worktreeExclusiveLocks holds worktrees exclusively within the process.
var worktreeExclusiveLocks sync.Map // absolute worktree git dir -> chan struct{} (1 slot; full = held)

// wtExclusiveKey is the context key marking that the context's call chain holds
// LockWorktree for the worktree whose absolute git dir is the key's value.
type wtExclusiveKey string

// worktreeGitDir returns the absolute git dir of the worktree at dir, symlinks resolved.
func worktreeGitDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir in %s printed nothing", dir)
	}
	if resolved, err := filepath.EvalSymlinks(out); err == nil {
		out = resolved
	}
	return out, nil
}

// LockWorktree holds the worktree at dir exclusively while the caller rebases, merges or
// commits there: an in-process lock keyed by the worktree's absolute git dir, then an
// exclusive flock on <worktree git dir>/mill-worktree.lock. Both waits give up when ctx is
// done. The returned context marks the lock as held; a nested LockWorktree for the same
// worktree made with it returns that context and a no-op unlock, so the lock is re-entrant
// along a call chain. Call unlock (safe to call more than once) when done.
func LockWorktree(ctx context.Context, dir string) (context.Context, func(), error) {
	return lockWorktreePoll(ctx, dir, worktreeLockPoll)
}

// lockWorktreePoll is LockWorktree with the flock retry interval given.
func lockWorktreePoll(ctx context.Context, dir string, poll time.Duration) (context.Context, func(), error) {
	gitDir, err := worktreeGitDir(ctx, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("lock worktree %s: find its git dir: %w", dir, err)
	}
	key := wtExclusiveKey(gitDir)
	if ctx.Value(key) != nil {
		return ctx, func() {}, nil
	}
	path := filepath.Join(gitDir, worktreeExclusiveFile)

	v, _ := worktreeExclusiveLocks.LoadOrStore(gitDir, make(chan struct{}, 1))
	mu := v.(chan struct{})
	select {
	case mu <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("lock worktree %s: %w; another task step in this mill is still holding %s", dir, ctx.Err(), path)
	}
	f, err := flockFile(ctx, path, poll)
	if err != nil {
		<-mu
		return nil, nil, fmt.Errorf("lock worktree %s: %w; another mill may be stuck holding %s", dir, err, path)
	}
	var once sync.Once
	return context.WithValue(ctx, key, true), func() {
		once.Do(func() {
			// Closing the descriptor releases the flock too; unlock first so the release
			// does not depend on close succeeding.
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			<-mu
		})
	}, nil
}

// ClearStaleIndexLock removes <worktree git dir>/index.lock of the worktree at dir when a
// crashed git left it behind. It removes the file only when ctx holds LockWorktree for dir
// (otherwise it returns an error), the file is older than minAge by now, and no running git
// process has the worktree as its working directory. When it cannot tell whether a git
// process runs there it removes nothing and returns an error. A removal is logged with logf.
// It reports whether it removed the file; a missing index.lock is not an error.
func ClearStaleIndexLock(ctx context.Context, dir string, minAge time.Duration, now func() time.Time, logf func(string, ...any)) (removed bool, err error) {
	return clearStaleIndexLock(ctx, dir, minAge, now, logf, gitProcessIn)
}

// clearStaleIndexLock is ClearStaleIndexLock with the git process check given: running
// reports whether a git process has its working directory in one of dirs.
func clearStaleIndexLock(ctx context.Context, dir string, minAge time.Duration, now func() time.Time, logf func(string, ...any), running func(dirs []string) (bool, error)) (bool, error) {
	gitDir, err := worktreeGitDir(ctx, dir)
	if err != nil {
		return false, fmt.Errorf("clear stale index.lock in %s: find its git dir: %w", dir, err)
	}
	path := filepath.Join(gitDir, "index.lock")
	if ctx.Value(wtExclusiveKey(gitDir)) == nil {
		return false, fmt.Errorf("clear stale %s: the worktree %s is not locked; take LockWorktree first and pass its context", path, dir)
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("clear stale %s: %w", path, err)
	}
	age := now().Sub(info.ModTime())
	if age <= minAge {
		return false, nil
	}
	top, err := Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, fmt.Errorf("clear stale %s: find the worktree top: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	busy, err := running([]string{top, gitDir})
	if err != nil {
		return false, fmt.Errorf("cannot tell whether a git process holds %s; remove it by hand if none runs: %w", path, err)
	}
	if busy {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("remove stale %s: %w", path, err)
	}
	logf("removed stale %s (age %s, no git process in %s)", path, age.Round(time.Millisecond), dir)
	return true, nil
}

// gitProcessIn reports whether a running git process has its working directory in one of
// dirs (or below). dirs must be absolute with symlinks resolved. It fails when it cannot tell.
func gitProcessIn(dirs []string) (bool, error) {
	switch runtime.GOOS {
	case "linux":
		return gitProcessInProc(dirs)
	case "darwin":
		return gitProcessInLsof(dirs)
	default:
		return false, fmt.Errorf("no way to list process working directories on %s", runtime.GOOS)
	}
}

// pathIn reports whether p is one of dirs or below one of them.
func pathIn(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// gitProcessInProc is gitProcessIn on Linux: it reads /proc/<pid>/comm and /proc/<pid>/cwd.
func gitProcessInProc(dirs []string) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("list /proc: %w", err)
	}
	gone := func(err error) bool {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if gone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read the name of process %s: %w", e.Name(), err)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(comm)), "git") {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if gone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read the working directory of git process %s: %w", e.Name(), err)
		}
		if pathIn(cwd, dirs) {
			return true, nil
		}
	}
	return false, nil
}

// gitProcessInLsof is gitProcessIn on macOS: it lists the working directories of processes
// whose command starts with "git" with lsof.
func gitProcessInLsof(dirs []string) (bool, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		if _, serr := os.Stat("/usr/sbin/lsof"); serr != nil {
			return false, fmt.Errorf("lsof not found: %w", err)
		}
		lsof = "/usr/sbin/lsof"
	}
	cmd := exec.Command(lsof, "-n", "-P", "-w", "-a", "-c", "git", "-d", "cwd", "-F", "pn")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		// lsof exits 1 when it lists nothing; anything else, or a complaint, is a failure.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout.Len() != 0 || stderr.Len() != 0 {
			return false, fmt.Errorf("lsof: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return false, nil
	}
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "n") && pathIn(line[1:], dirs) {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("read lsof output: %w", err)
	}
	return false, nil
}
