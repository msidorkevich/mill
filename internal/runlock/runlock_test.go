package runlock

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := Acquire(path)
		if err == nil {
			l.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock still held after the holder was killed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
