package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mill/internal/budget"
	"mill/internal/host"
	"mill/internal/price"
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

func TestRecoveryRecordsUsageFromStreamFiles(t *testing.T) {
	ctx := context.Background()
	s, id := recoverStore(t)
	dir := t.TempDir()
	stream := filepath.Join(dir, "implementer.stream.jsonl")
	lines := `{"type":"system","subtype":"init","model":"test-model"}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":1000,"output_tokens":234}}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":1000,"output_tokens":234}}}
`
	if err := os.WriteFile(stream, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, id, 0, "claude", stream); err != nil {
		t.Fatal(err)
	}
	id2, err := s.StartAttempt(ctx, "t1", 3, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, id2, 0, "claude", filepath.Join(dir, "missing.jsonl")); err != nil {
		t.Fatal(err)
	}
	prices := map[string]price.Rates{"test-model": {Input: 1000}}
	var out bytes.Buffer
	usage, err := interruptedUsage(ctx, &out, s, prices)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecoverInterrupted(ctx, usage); err != nil {
		t.Fatal(err)
	}
	if want := "recorded 1,234 tokens (billable, ~$1.00) from 1 interrupted attempt(s)"; !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	atts, _ := s.Attempts(ctx, "t1")
	if len(atts) != 2 {
		t.Fatalf("attempts = %+v", atts)
	}
	got := atts[0]
	if got.Status != store.AttemptAbandoned || got.Usage.InputTokens != 1000 || got.Usage.OutputTokens != 234 ||
		got.Usage.CostUSD != 1.0 || !got.CostEstimated {
		t.Errorf("recovered attempt = %+v", got)
	}
	if atts[1].Usage != (budget.Usage{}) || atts[1].CostEstimated {
		t.Errorf("attempt without stream = %+v", atts[1])
	}
	if tu, _ := s.TaskUsage(ctx, "t1"); tu.InputTokens != 1000 {
		t.Errorf("TaskUsage = %+v", tu)
	}
}

func TestRecoveryKillsChildrenOfExitedAgent(t *testing.T) {
	s, id := recoverStore(t)
	cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("child pid from %q: %v", out, err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL) })
	if err := s.SetAttemptProcess(context.Background(), id, pgid, "sh", "raw"); err != nil {
		t.Fatal(err)
	}
	if m, err := host.GroupMembers(pgid); err != nil || len(m) != 1 || m[0] != child {
		t.Fatalf("GroupMembers = %v, %v; want [%d]", m, err, child)
	}
	var buf bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &buf, s); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m, _ := host.GroupMembers(pgid); len(m) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child of exited agent still running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := "killed 1 leftover process(es) of worker " + strconv.Itoa(pgid) + " (t1 attempt 2 implementer), whose agent had already exited"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestGroupMembersEmptyForMissingGroup(t *testing.T) {
	gone := exec.Command("true")
	gone.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	m, err := host.GroupMembers(gone.Process.Pid)
	if err != nil || len(m) != 0 {
		t.Fatalf("GroupMembers = %v, %v; want none", m, err)
	}
}
