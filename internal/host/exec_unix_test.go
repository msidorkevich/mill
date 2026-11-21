//go:build unix

package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, ctx context.Context, spec ProcSpec) (Handle, Exit) {
	t.Helper()
	h, err := NewExec().Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ex, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return h, ex
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	dir := t.TempDir()
	out, errp := filepath.Join(dir, "logs", "out"), filepath.Join(dir, "logs", "err")
	_, ex := run(t, context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", "echo hello; echo oops >&2; exit 3"},
		Dir:        dir,
		StdoutPath: out,
		StderrPath: errp,
	})
	if ex.Code != 3 || ex.Signaled {
		t.Fatalf("exit = %+v, want code 3", ex)
	}
	if got := read(t, out); got != "hello\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := read(t, errp); got != "oops\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestExecRunsInDir(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	run(t, context.Background(), ProcSpec{Argv: []string{"/bin/pwd"}, Dir: dir, StdoutPath: out})
	if got := strings.TrimSpace(read(t, out)); got != dir {
		t.Fatalf("pwd = %q, want %q", got, dir)
	}
}

func TestExecStdin(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	if err := os.WriteFile(in, []byte("prompt text"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, context.Background(), ProcSpec{Argv: []string{"/bin/cat"}, StdinPath: in, StdoutPath: out})
	if got := read(t, out); got != "prompt text" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestExecDoesNotInheritEnvironment(t *testing.T) {
	t.Setenv("MILL_TEST_SECRET", "s3cret")
	dir := t.TempDir()

	// nil Env means an empty environment.
	out := filepath.Join(dir, "nil-env")
	run(t, context.Background(), ProcSpec{Argv: []string{"/usr/bin/env"}, StdoutPath: out})
	if got := read(t, out); got != "" {
		t.Fatalf("nil Env leaked variables: %q", got)
	}

	// The allowlist keeps PATH but drops the secret.
	out = filepath.Join(dir, "allow-env")
	run(t, context.Background(), ProcSpec{Argv: []string{"/usr/bin/env"}, Env: WorkerEnv(), StdoutPath: out})
	got := read(t, out)
	if strings.Contains(got, "MILL_TEST_SECRET") {
		t.Fatalf("secret leaked through WorkerEnv: %q", got)
	}
	if !strings.Contains(got, "PATH=") {
		t.Fatalf("PATH missing from WorkerEnv: %q", got)
	}

	// An explicit extra key is passed through.
	out = filepath.Join(dir, "extra-env")
	run(t, context.Background(), ProcSpec{Argv: []string{"/usr/bin/env"}, Env: WorkerEnv("MILL_TEST_SECRET"), StdoutPath: out})
	if !strings.Contains(read(t, out), "MILL_TEST_SECRET=s3cret") {
		t.Fatal("extra allowlisted key not passed")
	}
}

func TestAllowEnv(t *testing.T) {
	got := AllowEnv([]string{"A=1", "B=2", "C=3=x", "MALFORMED"}, []string{"C", "A", "MALFORMED"})
	want := []string{"A=1", "C=3=x"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowEnv = %v, want %v", got, want)
	}
}

// Kill must stop the process and the children it started.
func TestExecKillStopsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	h, err := NewExec().Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, pidFile)

	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	ex, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	waitGone(t, childPID)
}

func TestExecContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := NewExec().Start(ctx, ProcSpec{Argv: []string{"/bin/sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	ex, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the process promptly")
	}
}

func TestExecStartErrors(t *testing.T) {
	if _, err := NewExec().Start(context.Background(), ProcSpec{}); err == nil {
		t.Fatal("empty argv accepted")
	}
	if _, err := NewExec().Start(context.Background(), ProcSpec{Argv: []string{"/nonexistent/binary"}}); err == nil {
		t.Fatal("missing binary accepted")
	}
	if _, err := NewExec().Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, StdinPath: "/nonexistent/in"}); err == nil {
		t.Fatal("missing stdin file accepted")
	}
}

func waitForPID(t *testing.T, p string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(p); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child never wrote its pid")
	return 0
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d still running after Kill", pid)
}

func TestProcessInfoAndKillGroup(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() { cmd.Process.Kill(); <-exited }()

	pgid, command, alive, err := ProcessInfo(pid)
	if err != nil || !alive || pgid != pid || !strings.Contains(command, "sleep 30") {
		t.Fatalf("ProcessInfo = %d %q %v %v", pgid, command, alive, err)
	}
	if err := KillGroup(pgid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("process survived KillGroup")
	}
	if _, _, alive, err := ProcessInfo(pid); err != nil || alive {
		t.Fatalf("ProcessInfo after kill: alive=%v err=%v", alive, err)
	}
}

func TestProcessInfoMissingPidIsNotAlive(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, alive, err := ProcessInfo(cmd.Process.Pid); err != nil || alive {
		t.Fatalf("alive=%v err=%v, want not alive and no error", alive, err)
	}
}

func TestProcessCwdReportsWorkingDirectory(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	got, err := ProcessCwd(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = filepath.EvalSymlinks(got); got != dir {
		t.Fatalf("ProcessCwd = %q, want %q", got, dir)
	}

	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessCwd(gone.Process.Pid); err == nil {
		t.Fatal("ProcessCwd of a missing pid succeeded")
	}
}
