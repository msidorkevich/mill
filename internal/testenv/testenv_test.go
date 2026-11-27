package testenv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestPgrunArgvRunsTheCommandInANewProcessGroup(t *testing.T) {
	t.Parallel()
	argv := append(PgrunArgv(), "sh", "-c", `echo "$$ $(ps -o pgid= -p $$) ${`+pgrunEnv+`:-unset}"`)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	f := strings.Fields(string(out))
	if len(f) != 3 || f[0] != f[1] || f[2] != "unset" {
		t.Fatalf("output = %q, want \"<pid> <same pgid> unset\"", out)
	}
}

func TestPgrunHelperFailsWith127ForAMissingCommand(t *testing.T) {
	t.Parallel()
	argv := append(PgrunArgv(), "testenv-no-such-command")
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 127 {
		t.Fatalf("exit = %v, want status 127\n%s", err, out)
	}
	if !strings.Contains(string(out), "testenv-no-such-command") {
		t.Fatalf("output = %q, want the error naming the command", out)
	}
}

func TestGroupsScriptRunsBodyInItsOwnGroupUnderDashWithoutATerminal(t *testing.T) {
	t.Parallel()
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is not installed, so the script cannot be run under it")
	}
	checkScriptGroup(t, dash)
}

func TestGroupsScriptRunsBodyInItsOwnGroupUnderShWithoutATerminal(t *testing.T) {
	t.Parallel()
	checkScriptGroup(t, "/bin/sh")
}

// checkScriptGroup runs a Groups.Script under shell -c in a new session with stdin from
// /dev/null, so no terminal is attached, and checks that the body runs in the recorded
// process group of its own and that the group kill at cleanup stops the body and its child.
func checkScriptGroup(t *testing.T, shell string) {
	t.Helper()
	pids := filepath.Join(t.TempDir(), "pids")
	latch := t.TempDir()
	body := "sleep 300 >/dev/null 2>&1 & echo \"$$ $!\" > " + Quote(pids+".tmp") +
		" && mv " + Quote(pids+".tmp") + " " + Quote(pids) + "; " + WaitForFileScript(latch, "never")
	var out bytes.Buffer
	var done chan error
	var bodyPid, childPid int
	t.Run("holder", func(st *testing.T) {
		g := NewGroups(st)
		stdin, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		cmd := exec.Command(shell, "-c", g.Script(body))
		cmd.Stdin = stdin
		cmd.Stdout = &out
		cmd.Stderr = &out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sid := cmd.Process.Pid
		t.Cleanup(func() {
			if err := syscall.Kill(-sid, syscall.SIGKILL); err != nil && !groupGone(err) {
				t.Error(err)
			}
		})
		done = make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		var recorded []os.DirEntry
		var b []byte
		deadline := time.Now().Add(10 * time.Second)
		for {
			recorded, err = os.ReadDir(g.dir)
			if err != nil {
				st.Fatal(err)
			}
			b, _ = os.ReadFile(pids)
			if len(recorded) == 1 && len(b) > 0 {
				break
			}
			if time.Now().After(deadline) {
				st.Fatalf("after 10s: recorded groups %v, body pids %q; output:\n%s", recorded, b, out.String())
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := fmt.Sscan(string(b), &bodyPid, &childPid); err != nil {
			st.Fatalf("body pids %q: %v", b, err)
		}
		pgid, err := strconv.Atoi(recorded[0].Name())
		if err != nil {
			st.Fatalf("recorded group %q: %v", recorded[0].Name(), err)
		}
		if pgid != bodyPid {
			st.Errorf("recorded group %d, want the body's pid %d", pgid, bodyPid)
		}
		bodyGroup, err := syscall.Getpgid(bodyPid)
		if err != nil {
			st.Fatalf("getpgid(body %d): %v", bodyPid, err)
		}
		if bodyGroup != pgid {
			st.Errorf("body's group = %d, want the recorded %d", bodyGroup, pgid)
		}
		if own := syscall.Getpgrp(); bodyGroup == own {
			st.Errorf("body's group %d is the test's own group", bodyGroup)
		}
		if childGroup, err := syscall.Getpgid(childPid); err != nil || childGroup != pgid {
			st.Errorf("child's group = %d (%v), want %d", childGroup, err, pgid)
		}
		stillRunning(st, done)
	})
	if bodyPid == 0 {
		return
	}
	err := waitExit(t, done, "its test ended")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Errorf("exit = %v, want the killed body's failure", err)
	}
	waitGone(t, "body", bodyPid)
	waitGone(t, "body's child", childPid)
	for _, s := range []string{"job control", "tty"} {
		if strings.Contains(strings.ToLower(out.String()), s) {
			t.Errorf("output mentions %q:\n%s", s, out.String())
		}
	}
}

// waitGone polls until pid no longer exists, failing the test after a deadline.
func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still alive 10s after the group kill", what, pid)
		}
		time.Sleep(time.Millisecond)
	}
}
