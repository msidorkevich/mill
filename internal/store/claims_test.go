package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// claimStore opens a store whose claim liveness check reports the pids in dead as dead
// and every other pid as alive.
func claimStore(t *testing.T, dead map[int]bool, ids ...string) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "mill.db"), Options{
		Now:        func() time.Time { return fixed },
		ClaimAlive: func(c TaskClaim) (bool, error) { return !dead[c.PID], nil },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	for _, id := range ids {
		if err := s.CreateTask(context.Background(), spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func wantBusy(t *testing.T, err error, task, doing string) {
	t.Helper()
	var be *BusyError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v; want *BusyError", err)
	}
	if be.TaskID != task || be.Doing != doing {
		t.Fatalf("BusyError = %+v; want task %q doing %q", be, task, doing)
	}
	msg := be.Error()
	for _, w := range []string{task, doing, "retry"} {
		if !strings.Contains(msg, w) {
			t.Errorf("Error() = %q; want it to mention %q", msg, w)
		}
	}
}

func TestClaimTasksOnIdleTasksListsThemAsClaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, nil, "a", "b")
	c := Claimer{Owner: "o1", PID: 101, Command: "run", Executable: "mill"}
	if err := s.ClaimTasks(ctx, c, []string{"a", "b"}); err != nil {
		t.Fatalf("ClaimTasks: %v", err)
	}
	got, err := s.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ClaimedTasks = %+v; want a and b", got)
	}
	for _, id := range []string{"a", "b"} {
		cl := got[id]
		if cl.TaskID != id || cl.Owner != "o1" || cl.PID != 101 || cl.Command != "run" || !cl.ClaimedAt.Equal(fixed) {
			t.Errorf("claim on %s = %+v", id, cl)
		}
	}
}

func TestClaimTasksRefusesTaskWithRunningAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, nil, "a")
	if err := s.SetTaskStatus(ctx, "a", TaskRunning, PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "a", 2, "implementer"); err != nil {
		t.Fatal(err)
	}
	err := s.ClaimTasks(ctx, Claimer{Owner: "o1", PID: 1}, []string{"a"})
	wantBusy(t, err, "a", "attempt 2 (implementer) running")
}

func TestClaimTasksRefusesRunningTaskWithoutAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, nil, "a")
	if err := s.SetTaskStatus(ctx, "a", TaskRunning, PhaseGate, ""); err != nil {
		t.Fatal(err)
	}
	err := s.ClaimTasks(ctx, Claimer{Owner: "o1", PID: 1}, []string{"a"})
	wantBusy(t, err, "a", "running (gate)")
}

func TestClaimTasksRefusesSecondOwnerInSameProcessUntilReleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, nil, "a")
	first, second := NewClaimer("run"), NewClaimer("task run")
	if first.Owner == second.Owner || first.PID != os.Getpid() {
		t.Fatalf("NewClaimer owners %q, %q pid %d", first.Owner, second.Owner, first.PID)
	}
	if err := s.ClaimTasks(ctx, first, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	err := s.ClaimTasks(ctx, second, []string{"a"})
	wantBusy(t, err, "a", "claimed by mill run (pid "+strconv.Itoa(os.Getpid())+")")

	// Releasing as the second owner does not drop the first owner's claim.
	if err := s.ReleaseClaims(ctx, second, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTasks(ctx, second, []string{"a"}); err == nil {
		t.Fatal("second claimer took the task after releasing a claim it did not hold")
	}
	if err := s.ReleaseClaims(ctx, first, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTasks(ctx, second, []string{"a"}); err != nil {
		t.Fatalf("ClaimTasks after release: %v", err)
	}
	got, _ := s.ClaimedTasks(ctx)
	if got["a"].Owner != second.Owner {
		t.Errorf("claim on a = %+v; want owner %s", got["a"], second.Owner)
	}
}

func TestClaimTasksClaimsNoneWhenOneIsBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, nil, "a", "b", "c")
	if err := s.SetTaskStatus(ctx, "b", TaskRunning, PhaseVerify, ""); err != nil {
		t.Fatal(err)
	}
	err := s.ClaimTasks(ctx, Claimer{Owner: "o1", PID: 1, Command: "run"}, []string{"a", "b", "c"})
	wantBusy(t, err, "b", "running (verify)")
	got, err := s.ClaimedTasks(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("ClaimedTasks = %+v, %v; want none", got, err)
	}
}

func TestClaimTasksTakesOverClaimOfDeadProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, map[int]bool{200: true}, "a")
	if err := s.ClaimTasks(ctx, Claimer{Owner: "dead", PID: 200, Command: "run"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimedTasks(ctx); len(got) != 0 {
		t.Errorf("ClaimedTasks lists a dead claim: %+v", got)
	}
	if err := s.ClaimTasks(ctx, Claimer{Owner: "live", PID: 300, Command: "task run"}, []string{"a"}); err != nil {
		t.Fatalf("ClaimTasks over a dead claim: %v", err)
	}
	got, _ := s.ClaimedTasks(ctx)
	if got["a"].Owner != "live" || got["a"].PID != 300 || got["a"].Command != "task run" {
		t.Errorf("claim on a = %+v; want owner live", got["a"])
	}
}

func TestRecoverInterruptedRemovesDeadClaimsAndKeepsLiveOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := claimStore(t, map[int]bool{200: true}, "a", "b")
	if err := s.ClaimTasks(ctx, Claimer{Owner: "dead", PID: 200}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTasks(ctx, Claimer{Owner: "live", PID: 300}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecoverInterrupted(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT task_id, owner FROM task_claims ORDER BY task_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			t.Fatal(err)
		}
		got = append(got, id+":"+owner)
	}
	if strings.Join(got, ",") != "b:live" {
		t.Errorf("task_claims rows = %v; want [b:live]", got)
	}
}

func TestClaimTasksTakesOverClaimWhosePidNoLongerRunsMill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The default liveness check: a live process that is not the claiming command.
	s := newStore(t)
	for _, id := range []string{"a", "b"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	pid := cmd.Process.Pid

	// A claim recorded by "mill run" whose pid now runs sleep: taken over.
	if err := s.ClaimTasks(ctx, Claimer{Owner: "reused", PID: pid, Command: "run", Executable: "mill"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTasks(ctx, NewClaimer("run"), []string{"a"}); err != nil {
		t.Fatalf("ClaimTasks over a reused pid: %v", err)
	}

	// A claim whose pid still runs the recorded command: refused.
	if err := s.ClaimTasks(ctx, Claimer{Owner: "same", PID: pid, Command: "60", Executable: "sleep"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	err := s.ClaimTasks(ctx, NewClaimer("run"), []string{"b"})
	wantBusy(t, err, "b", "claimed by mill 60 (pid "+strconv.Itoa(pid)+")")
}

func TestRunsClaimerMatchesExecutableAndCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cmdline, exe, command string
		want                  bool
	}{
		{"/usr/local/bin/mill run --goal g1", "/usr/local/bin/mill", "run", true},
		{"mill task run g1/a", "mill", "task run", true},
		{"mill", "mill", "", true},
		{"/usr/bin/vim notes", "mill", "run", false},
		{"mill status", "mill", "run", false},
		{"millx run", "mill", "run", false},
	} {
		if got := runsClaimer(tc.cmdline, tc.exe, tc.command); got != tc.want {
			t.Errorf("runsClaimer(%q, %q, %q) = %v; want %v", tc.cmdline, tc.exe, tc.command, got, tc.want)
		}
	}
}
