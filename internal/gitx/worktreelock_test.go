package gitx

import (
	"context"
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

// wtAdminHelperEnv names the repository the helper process adds and removes worktrees in.
// wtAdminHelperDirEnv names the directory it puts them in.
const (
	wtAdminHelperEnv    = "MILL_GITX_WTADMIN_HELPER_REPO"
	wtAdminHelperDirEnv = "MILL_GITX_WTADMIN_HELPER_DIR"
	wtAdminRounds       = 12
	// wtAdminHelperDone is printed by the helper once all its rounds succeeded.
	wtAdminHelperDone = "wtadmin-helper-done"
)

// wtAdminHelperMain is the helper process: it adds and removes worktrees in a loop and
// returns the exit code.
func wtAdminHelperMain() int {
	repo, dir := os.Getenv(wtAdminHelperEnv), os.Getenv(wtAdminHelperDirEnv)
	if err := wtAdminCycle(context.Background(), repo, dir, "helper", wtAdminRounds); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(wtAdminHelperDone)
	return 0
}

// wtAdminCycle adds a worktree on a new branch, adds a detached one, removes both and prunes,
// rounds times. Names start with prefix so concurrent callers never collide.
func wtAdminCycle(ctx context.Context, repo, dir, prefix string, rounds int) error {
	for i := range rounds {
		name := fmt.Sprintf("%s-%d", prefix, i)
		wt := filepath.Join(dir, name)
		detached := filepath.Join(dir, name+"-detached")
		if err := AddWorktree(ctx, repo, wt, "wt/"+name, "main"); err != nil {
			return err
		}
		if err := AddDetachedWorktree(ctx, repo, detached, "main"); err != nil {
			return err
		}
		if err := RemoveWorktree(ctx, repo, wt); err != nil {
			return err
		}
		if err := RemoveWorktree(ctx, repo, detached); err != nil {
			return err
		}
		if err := PruneWorktrees(ctx, repo); err != nil {
			return err
		}
	}
	return nil
}

// wtAdminAssertClean fails unless `git worktree list` shows only the main work tree.
func wtAdminAssertClean(t *testing.T, repo string) {
	t.Helper()
	list := mustRun(t, context.Background(), repo, "worktree", "list", "--porcelain")
	if n := strings.Count(list, "worktree "); n != 1 {
		t.Fatalf("git worktree list shows %d work trees, want only the main one:\n%s", n, list)
	}
}

// wtAdminLockPath returns the worktree admin lock file of repo.
func wtAdminLockPath(t *testing.T, repo string) string {
	t.Helper()
	dir, err := CommonDir(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, worktreeLockFile)
}

func TestConcurrentWorktreeAddRemoveOneRepo(t *testing.T) {
	t.Parallel()
	r := repo(t)
	dir := t.TempDir()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := wtAdminCycle(ctx, r, dir, fmt.Sprintf("g%d", g), 4); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	wtAdminAssertClean(t, r)
}

func TestWorktreeAdminLockAcrossProcesses(t *testing.T) {
	t.Parallel()
	r := repo(t)
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), wtAdminHelperEnv+"="+r, wtAdminHelperDirEnv+"="+dir)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ownErr := wtAdminCycle(ctx, r, dir, "test", wtAdminRounds)
	if err := cmd.Wait(); err != nil {
		t.Errorf("helper process: %v\n%s", err, out.String())
	} else if !strings.Contains(out.String(), wtAdminHelperDone) {
		t.Errorf("helper process did no worktree work (no %q in its output):\n%s", wtAdminHelperDone, out.String())
	}
	// Both processes worked in dir: the helper's branches exist only if it ran its rounds.
	if refs := mustRun(t, context.Background(), r, "branch", "--list", "wt/helper-*"); strings.Count(refs, "wt/helper-") != wtAdminRounds {
		t.Errorf("helper created branches %q, want %d", refs, wtAdminRounds)
	}
	if ownErr != nil {
		t.Fatal(ownErr)
	}
	wtAdminAssertClean(t, r)
}

func TestWorktreeAdminLockWaitHonoursContext(t *testing.T) {
	t.Parallel()
	r := repo(t)
	lockPath := wtAdminLockPath(t, r)

	t.Run("flock held by another descriptor", func(t *testing.T) {
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		unlock, err := lockWorktreeAdminPoll(ctx, r, time.Millisecond)
		if err == nil {
			unlock()
			t.Fatal("lock taken while another descriptor holds the flock")
		}
		if !strings.Contains(err.Error(), lockPath) {
			t.Errorf("error %q does not name the lock file %s", err, lockPath)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("wait took %v after a 200ms deadline", d)
		}
		// The in-process mutex was released on failure: once the flock is free the lock
		// is taken again.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		unlock, err = lockWorktreeAdminPoll(context.Background(), r, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	})

	t.Run("mutex held in this process", func(t *testing.T) {
		unlock, err := lockWorktreeAdminPoll(context.Background(), r, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := AddWorktree(ctx, r, filepath.Join(t.TempDir(), "wt"), "blocked", "main"); err == nil {
			t.Fatal("worktree added while the lock is held")
		} else if !strings.Contains(err.Error(), lockPath) {
			t.Errorf("error %q does not name the lock file %s", err, lockPath)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("wait took %v after a 200ms deadline", d)
		}
	})
}
