package testenv

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDisableGitMaintenanceAppendsToExistingGitConfigEntries(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "tester")
	// Registered so the entries DisableGitMaintenance adds are undone after the test.
	for _, k := range []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if err := DisableGitMaintenance(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "3" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 3", got)
	}
	dir := t.TempDir()
	for key, want := range map[string]string{"user.name": "tester", "maintenance.auto": "false", "gc.auto": "0"} {
		cmd := exec.Command("git", "config", "--get", key)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git config --get %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("git config %s = %q, want %q", key, got, want)
		}
	}
}

func TestDisableGitMaintenanceRejectsABadCount(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "many")
	err := DisableGitMaintenance()
	if err == nil || !strings.Contains(err.Error(), `GIT_CONFIG_COUNT="many"`) {
		t.Fatalf("err = %v, want one naming the bad GIT_CONFIG_COUNT", err)
	}
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "many" {
		t.Fatalf("GIT_CONFIG_COUNT = %q after the error, want it unchanged", got)
	}
}

// startShell runs script with sh in its own process group, killed at cleanup, and returns a
// channel that receives its exit error.
func startShell(t *testing.T, script string) <-chan error {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !groupGone(err) {
			t.Error(err)
		}
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

// waitExit polls every millisecond until the shell exits, failing the test after a deadline.
func waitExit(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			return err
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell is still alive 10s after %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stillRunning fails the test if the shell has already exited.
func stillRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the wait script ended before it was released: %v", err)
	default:
	}
}

func TestWaitScriptExitsWhenDirectoryRemoved(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "latch")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	done := startShell(t, WaitForFileScript(dir, "release")+"; echo released")
	stillRunning(t, done)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	err := waitExit(t, done, "its directory was removed")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1 (gave up)", err)
	}
}

func TestWaitScriptContinuesWhenAnyFileAppears(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	done := startShell(t, WaitForFileScript(dir, "one", "all")+"; echo released > "+Quote(out))
	stillRunning(t, done)
	if err := os.WriteFile(filepath.Join(dir, "all"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, "the file appeared"); err != nil {
		t.Fatalf("exit = %v, want success", err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "released\n" {
		t.Fatalf("output = %q (%v), want the rest of the script to run", b, err)
	}
}

func TestGroupsKillTheirShellsAtCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var done <-chan error
	outer := t
	t.Run("holder", func(t *testing.T) {
		g := NewGroups(t)
		// The wrapper shell belongs to the outer test, so only g's cleanup can end the job.
		done = startShell(outer, g.Script(WaitForFileScript(dir, "never")))
		deadline := time.Now().Add(10 * time.Second)
		for {
			if entries, err := os.ReadDir(g.dir); err != nil {
				t.Fatal(err)
			} else if len(entries) == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the job's process group was never recorded")
			}
			time.Sleep(time.Millisecond)
		}
		stillRunning(t, done)
	})
	// The subtest's cleanup killed the waiting job; its wrapper shell then ends too, while
	// dir still exists and nothing was released.
	err := waitExit(t, done, "its test ended")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("exit = %v, want the killed job's failure", err)
	}
}
