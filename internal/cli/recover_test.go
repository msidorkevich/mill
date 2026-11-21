package cli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mill/internal/host"
	"mill/internal/store"
	"mill/internal/taskspec"
)

func recoverStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "mill.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateTask(ctx, taskspec.Spec{ID: "t1", Title: "t", Kind: "implement"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "t1", 2, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func startSleeper(t *testing.T) (*exec.Cmd, chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	return cmd, done
}

func TestRecoveryKillsLeftoverWorker(t *testing.T) {
	s, id := recoverStore(t)
	cmd, done := startSleeper(t)
	if err := s.SetAttemptProcess(context.Background(), id, cmd.Process.Pid, "sleep", "raw"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("leftover worker still running")
	}
	want := "killed leftover worker " + strconv.Itoa(cmd.Process.Pid) + " (t1 attempt 2 implementer)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestRecoveryLeavesMismatchedAndMissingProcessesAlone(t *testing.T) {
	s, id := recoverStore(t)
	cmd, done := startSleeper(t)
	// Command does not match: the pid was reused by something else.
	if err := s.SetAttemptProcess(context.Background(), id, cmd.Process.Pid, "claude", "raw"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("unrelated process was killed")
	case <-time.After(300 * time.Millisecond):
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}

	// A pid that no longer exists.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(context.Background(), id, gone.Process.Pid, "true", "raw"); err != nil {
		t.Fatal(err)
	}
	if _, _, alive, _ := host.ProcessInfo(gone.Process.Pid); alive {
		t.Skip("pid was reused immediately")
	}
	if err := killLeftoverWorkers(context.Background(), &out, s); err != nil || out.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, out.String())
	}
}
