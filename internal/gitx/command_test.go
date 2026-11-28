package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cancelFakeGit installs a shell script as the git binary for the test and restores the real
// one and the grace period afterwards.
func cancelFakeGit(t *testing.T, script string, grace time.Duration) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prevBin, prevGrace := Bin(), gracePeriod
	binPath, gracePeriod = path, grace
	t.Cleanup(func() { binPath, gracePeriod = prevBin, prevGrace })
}

// cancelRepo returns a temp dir with a .git directory for the fake git to lock.
func cancelRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

// cancelWaitFor polls until path exists, failing the test at the deadline.
func cancelWaitFor(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("fake git exited before it started: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	t.Fatalf("fake git never wrote %s", path)
}

// cancelResult waits for the git call to return, failing the test at the deadline.
func cancelResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("git call did not return after cancellation")
		return nil
	}
}

func TestCancelSendsSigtermAndGitRemovesItsLock(t *testing.T) {
	marks := t.TempDir()
	started, termed := filepath.Join(marks, "started"), filepath.Join(marks, "termed")
	cancelFakeGit(t, `#!/bin/sh
trap 'echo term > "`+termed+`"; rm -f .git/index.lock; kill $pid; exit 143' TERM
: > .git/index.lock
sleep 60 </dev/null >/dev/null 2>&1 &
pid=$!
: > "`+started+`"
wait $pid
`, 5*time.Second)
	repo := cancelRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, repo, "commit", "-m", "x")
		done <- err
	}()
	cancelWaitFor(t, started, done)
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); err != nil {
		t.Fatalf("fake git did not take the lock: %v", err)
	}
	cancel()
	err := cancelResult(t, done)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "git commit -m x") {
		t.Errorf("err = %v, want it to name the git command", err)
	}
	if b, rerr := os.ReadFile(termed); rerr != nil || strings.TrimSpace(string(b)) != "term" {
		t.Errorf("fake git did not get SIGTERM: %q, %v", b, rerr)
	}
	if _, serr := os.Stat(filepath.Join(repo, ".git", "index.lock")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("index.lock left behind after cancellation: %v", serr)
	}
}

func TestCancelKillsGitThatIgnoresSigtermAfterGracePeriod(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	cancelFakeGit(t, `#!/bin/sh
trap '' TERM
: > "`+started+`"
exec sleep 60 </dev/null >/dev/null 2>&1
`, 5*time.Millisecond)
	repo := cancelRepo(t)

	for _, tc := range []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"run", func(ctx context.Context) error { _, err := Run(ctx, repo, "status"); return err }},
		{"run exit", func(ctx context.Context) error { _, _, err := runExit(ctx, repo, "diff", "--quiet"); return err }},
		{"is ancestor", func(ctx context.Context) error { _, err := IsAncestor(ctx, repo, "a", "b"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(started); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx) }()
			cancelWaitFor(t, started, done)
			cancel()
			err := cancelResult(t, done)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if !strings.Contains(err.Error(), "killed") {
				t.Errorf("err = %v, want git killed after the grace period", err)
			}
		})
	}
}

func TestCancelReportsDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := withCtxErr(ctx, errors.New("signal: killed"))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("err = %v, want the exec error wrapping context.DeadlineExceeded", err)
	}
	if _, err := Run(ctx, cancelRepo(t), "status"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run err = %v, want context.DeadlineExceeded", err)
	}
}

func TestCommandSetsGracePeriod(t *testing.T) {
	cmd := Command(context.Background(), "status")
	if cmd.WaitDelay != 2*time.Second || gracePeriod != 2*time.Second {
		t.Errorf("WaitDelay = %v, gracePeriod = %v, want 2s", cmd.WaitDelay, gracePeriod)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel is not set")
	}
	if cmd.Args[0] != Bin() {
		t.Errorf("Args[0] = %q, want %q", cmd.Args[0], Bin())
	}
}
