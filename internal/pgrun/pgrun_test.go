//go:build unix

package pgrun_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pgrun"
)

// helperEnv makes the test binary act as the pgrun helper: it runs Exec on its arguments.
const helperEnv = "MILL_PGRUN_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		err := pgrun.Exec(os.Args[1:])
		fmt.Fprintf(os.Stderr, "pgrun helper: %v\n", err)
		if errors.Is(err, pgrun.ErrNotFound) {
			os.Exit(127)
		}
		os.Exit(126)
	}
	os.Exit(m.Run())
}

// helper returns the path of the test binary, which acts as the helper under helperEnv.
func helper(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find test binary: %v", err)
	}
	return exe
}

// shellCommand runs script with shell -c, with the helper's path in $HELPER.
func shellCommand(t *testing.T, shell, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Env = append(os.Environ(), helperEnv+"=1", "HELPER="+helper(t))
	return cmd
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitGone polls until pid no longer exists or the deadline passes.
func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still alive after the group kill", what, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkGroupLeader starts the helper from shell, which runs sh that starts a background
// grandchild, and checks the process group and that one group kill stops both.
func checkGroupLeader(t *testing.T, shell string) {
	script := `"$HELPER" -- /bin/sh -c 'sleep 300 >/dev/null 2>&1 & echo "$$ $!"; wait'`
	cmd := shellCommand(t, shell, script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", shell, err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pids: %v (stderr: %q)", err, stderr.String())
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("child printed %q, want \"<pid> <grandchild pid>\"", line)
	}
	child, err1 := strconv.Atoi(fields[0])
	grandchild, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("child printed %q, want two pids", line)
	}

	pgid, err := syscall.Getpgid(child)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", child, err)
	}
	if pgid != child {
		t.Errorf("child pgid = %d, want its pid %d", pgid, child)
	}
	if own := syscall.Getpgrp(); pgid == own {
		t.Errorf("child pgid = %d, same as the test's pgid", pgid)
	}
	gpgid, err := syscall.Getpgid(grandchild)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", grandchild, err)
	}
	if gpgid != pgid {
		t.Errorf("grandchild pgid = %d, want the child's group %d", gpgid, pgid)
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -%d: %v", pgid, err)
	}
	waitErr := cmd.Wait()
	waited = true
	waitGone(t, "child", child)
	waitGone(t, "grandchild", grandchild)
	if waitErr != nil {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			t.Fatalf("wait %s: %v", shell, waitErr)
		}
	}

	low := strings.ToLower(stderr.String())
	for _, bad := range []string{"job control", "tty", "terminal", "setpgid", "pgrun helper"} {
		if strings.Contains(low, bad) {
			t.Errorf("stderr mentions %q: %q", bad, stderr.String())
		}
	}
}

func TestExecLeadsNewProcessGroupUnderDash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is not installed; the dash case needs it on PATH")
	}
	checkGroupLeader(t, dash)
}

func TestExecLeadsNewProcessGroupUnderSh(t *testing.T) {
	checkGroupLeader(t, "/bin/sh")
}

func TestExecPrintsNothingOnSuccess(t *testing.T) {
	cmd := shellCommand(t, "/bin/sh", `"$HELPER" -- /bin/sh -c 'exit 0'`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v (stderr: %q)", err, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("helper printed stdout %q, stderr %q; want nothing", stdout.String(), stderr.String())
	}
}

func TestExecPassesArgumentsWithoutDoubleDash(t *testing.T) {
	cmd := shellCommand(t, "/bin/sh", `"$HELPER" /bin/echo a "b c"`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := string(out); got != "a b c\n" {
		t.Errorf("output = %q, want %q", got, "a b c\n")
	}
}

func TestExecMissingCommandFailsWithItsName(t *testing.T) {
	shells := []string{"/bin/sh"}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	for _, shell := range shells {
		for _, name := range []string{"/nonexistent/mill-pgrun-missing", "mill-pgrun-missing-cmd"} {
			cmd := shellCommand(t, shell, `"$HELPER" -- `+name+` arg`)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 127 {
				t.Errorf("%s: %s: err = %v, want exit 127 (stderr: %q)", shell, name, err, stderr.String())
			}
			if !strings.Contains(stderr.String(), name) {
				t.Errorf("%s: stderr %q does not name the command %q", shell, stderr.String(), name)
			}
		}
	}
}

func TestExecWithoutCommandFails(t *testing.T) {
	for _, args := range [][]string{nil, {"--"}} {
		err := pgrun.Exec(args)
		if err == nil || !strings.Contains(err.Error(), "no command") {
			t.Errorf("Exec(%q) = %v, want a no-command error", args, err)
		}
	}
}

func TestExecMissingCommandReturnsErrNotFound(t *testing.T) {
	err := pgrun.Exec([]string{"--", "/nonexistent/mill-pgrun-missing"})
	if !errors.Is(err, pgrun.ErrNotFound) {
		t.Fatalf("Exec = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "/nonexistent/mill-pgrun-missing") {
		t.Errorf("error %q does not name the command", err)
	}
}
