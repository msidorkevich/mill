package runlock

import (
	"bufio"
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

func TestAcquireIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(path)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(os.Getpid())) || !strings.Contains(err.Error(), "another mill run is active") {
		t.Fatalf("second Acquire err = %v, want error naming pid %d", err, os.Getpid())
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestHelperHoldLock is run as a child process by TestKilledHolderFreesLock.
func TestHelperHoldLock(t *testing.T) {
	path := os.Getenv("RUNLOCK_TEST_PATH")
	if path == "" {
		t.Skip("helper process only")
	}
	if _, err := Acquire(path); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED")
	time.Sleep(time.Minute)
}

func TestKilledHolderFreesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "RUNLOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child did not lock: %q %v", line, err)
	}
	_, err = Acquire(path)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(cmd.Process.Pid)) {
		t.Fatalf("Acquire while child holds = %v, want error naming pid %d", err, cmd.Process.Pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock still held right after the holder was killed: %v", err)
	}
	l.Release()
}

func TestSecondAcquireInSameProcessFailsAndKeepsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := Acquire(path); err == nil || !strings.Contains(err.Error(), "already holds run lock") {
		t.Fatalf("second Acquire err = %v, want already-held error", err)
	}
	// A different process must still see the lock held.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "RUNLOCK_TEST_PATH="+path)
	out, err := cmd.Output()
	if err == nil || !strings.Contains(string(out), "ERR") || !strings.Contains(string(out), fmt.Sprint(os.Getpid())) {
		t.Fatalf("child Acquire = %q, %v; want failure naming pid %d", out, err, os.Getpid())
	}
}

// TestHelperHoldLockWithChild locks, starts a long-running child, then waits to be killed.
func TestHelperHoldLockWithChild(t *testing.T) {
	path := os.Getenv("RUNLOCK_TEST_PATH")
	if path == "" {
		t.Skip("helper process only")
	}
	if _, err := Acquire(path); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED", sleeper.Process.Pid)
	time.Sleep(time.Minute)
}

func TestChildOfKilledHolderDoesNotKeepLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLockWithChild$")
	cmd.Env = append(os.Environ(), "RUNLOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	f := strings.Fields(line)
	if err != nil || len(f) != 2 || f[0] != "LOCKED" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child did not lock: %q %v", line, err)
	}
	sleeperPid, _ := strconv.Atoi(f[1])
	defer syscall.Kill(sleeperPid, syscall.SIGKILL)
	cmd.Process.Kill()
	cmd.Wait()
	if err := syscall.Kill(sleeperPid, 0); err != nil {
		t.Fatalf("sleep child not running: %v", err)
	}
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire while holder's child still runs: %v", err)
	}
	l.Release()
}

func TestHeldFalseForMissingOrUnlockedFile(t *testing.T) {
	dir := t.TempDir()
	if pid, held, err := Held(filepath.Join(dir, "missing.lock")); err != nil || held || pid != 0 {
		t.Fatalf("Held(missing) = %d, %v, %v; want 0, false, nil", pid, held, err)
	}
	path := filepath.Join(dir, "run.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if pid, held, err := Held(path); err != nil || held || pid != 0 {
		t.Fatalf("Held(unlocked) = %d, %v, %v; want 0, false, nil", pid, held, err)
	}
}

func TestHeldReportsOwnProcessAndKeepsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	pid, held, err := Held(path)
	if err != nil || !held || pid != os.Getpid() {
		t.Fatalf("Held = %d, %v, %v; want %d, true, nil", pid, held, err, os.Getpid())
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second Acquire succeeded after Held; Held dropped the lock")
	}
}

func TestHeldReportsChildHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "RUNLOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("child did not lock: %q %v", line, err)
	}
	pid, held, err := Held(path)
	if err != nil || !held || pid != cmd.Process.Pid {
		t.Fatalf("Held = %d, %v, %v; want %d, true, nil", pid, held, err, cmd.Process.Pid)
	}
}

func TestHeldErrorsOnUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A path below a regular file cannot be opened; that is not "missing".
	if pid, held, err := Held(filepath.Join(file, "run.lock")); err == nil || held || pid != 0 {
		t.Fatalf("Held(unreadable) = %d, %v, %v; want an error", pid, held, err)
	}
}
