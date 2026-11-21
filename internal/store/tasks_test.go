package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mill/internal/budget"
	"mill/internal/taskspec"
)

func spec(id string) taskspec.Spec {
	return taskspec.Spec{
		ID: id, Title: "Title " + id, Kind: "implement", Spec: "do it",
		Acceptance: []string{"works"}, Checks: []string{"go test ./..."}, Owns: []string{"a.go"}, Size: "M",
	}
}

func TestTaskLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))

	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec("t1")); err == nil {
		t.Fatal("duplicate task id accepted")
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskPending || got.Size != "M" || got.Checks[0] != "go test ./..." || got.Owns[0] != "a.go" {
		t.Fatalf("task = %+v", got)
	}
	if !got.CreatedAt.Equal(fixed) {
		t.Fatalf("created_at = %v", got.CreatedAt)
	}

	if err := s.SetTaskStatus(ctx, "t1", TaskBlocked, PhaseImplement, "budget"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, "t1")
	if got.Status != TaskBlocked || got.Phase != PhaseImplement || got.Reason != "budget" {
		t.Fatalf("after status change: %+v", got)
	}
	evs, err := s.TaskEvents(ctx, "t1")
	if err != nil || len(evs) != 2 || evs[0].Kind != "task-created" || evs[1].Kind != "task-status" {
		t.Fatalf("events = %+v, %v", evs, err)
	}

	if _, err := s.GetTask(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task err = %v", err)
	}
	if err := s.SetTaskStatus(ctx, "missing", TaskFailed, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing status err = %v", err)
	}
}

func TestAttemptsAndUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MaxAttemptN(ctx, "t1"); n != 0 {
		t.Fatalf("MaxAttemptN = %d before any attempt", n)
	}
	a1, err := s.StartAttempt(ctx, "t1", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "t1", 1, "implementer"); err == nil {
		t.Fatal("duplicate (task, n, role) accepted")
	}
	if err := s.FinishAttempt(ctx, a1, "ok", budget.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.5}, "out.json", "log"); err != nil {
		t.Fatal(err)
	}
	a2, _ := s.StartAttempt(ctx, "t1", 1, "verifier")
	if err := s.FinishAttempt(ctx, a2, "ok", budget.Usage{InputTokens: 50, CacheReadTokens: 7, CostUSD: 0.25}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "t1", 2, "implementer"); err != nil {
		t.Fatal(err)
	}

	list, err := s.Attempts(ctx, "t1")
	if err != nil || len(list) != 3 {
		t.Fatalf("Attempts = %d, %v", len(list), err)
	}
	if list[0].Status != "ok" || list[0].OutputPath != "out.json" || list[0].EndedAt == nil || list[0].Usage.InputTokens != 100 {
		t.Fatalf("attempt 1 = %+v", list[0])
	}
	if list[2].Status != AttemptRunning || list[2].EndedAt != nil {
		t.Fatalf("attempt 3 = %+v", list[2])
	}
	u, err := s.TaskUsage(ctx, "t1")
	want := budget.Usage{InputTokens: 150, OutputTokens: 10, CacheReadTokens: 7, CostUSD: 0.75}
	if err != nil || u != want {
		t.Fatalf("TaskUsage = %+v, %v; want %+v", u, err, want)
	}
	if n, _ := s.MaxAttemptN(ctx, "t1"); n != 2 {
		t.Fatalf("MaxAttemptN = %d, want 2", n)
	}
}

func TestBranches(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBranch(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	b := Branch{TaskID: "t1", Name: "mill/t/t1", Base: "main", BaseSHA: "abc", Worktree: "/wt/t1"}
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.HeadSHA = "def"
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBranch(ctx, "t1")
	if err != nil || got.HeadSHA != "def" || got.Worktree != "/wt/t1" || got.BaseSHA != "abc" {
		t.Fatalf("branch = %+v, %v", got, err)
	}
}

// A database at schema v1 upgrades in place to the latest version.
func TestMigrateFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:1]
	LatestVersion = 1
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "init", "", "", nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion {
		t.Fatalf("schema = %d, want %d", v, LatestVersion)
	}
	if err := s2.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetTaskStatus(ctx, "t1", TaskRunning, PhaseGate, ""); err != nil {
		t.Fatalf("phase column missing after upgrade: %v", err)
	}
}

func TestUpdateTaskSpec(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	upd := spec("t1")
	upd.Spec = "do it better"
	upd.Checks = []string{"make check"}
	if err := s.UpdateTaskSpec(ctx, upd); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, "t1")
	if got.Spec.Spec != "do it better" || got.Checks[0] != "make check" || got.Status != TaskPending {
		t.Fatalf("task = %+v", got)
	}
	if err := s.UpdateTaskSpec(ctx, spec("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskSummaries(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))

	got, err := s.TaskSummaries(ctx, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: %+v, %v", got, err)
	}

	// Created out of order: b and a share a timestamp (id breaks the tie), c is older.
	s.now = func() time.Time { return fixed }
	for _, id := range []string{"b", "a"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return fixed.Add(-time.Hour) }
	if err := s.CreateTask(ctx, spec("c")); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixed }

	a1, _ := s.StartAttempt(ctx, "b", 1, "implementer")
	s.FinishAttempt(ctx, a1, "ok", budget.Usage{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 5, CacheReadTokens: 900, CostUSD: 0.5}, "", "")
	a2, _ := s.StartAttempt(ctx, "b", 2, "implementer")
	s.FinishAttempt(ctx, a2, "ok", budget.Usage{InputTokens: 50, CostUSD: 0.25}, "", "")
	if err := s.SetTaskStatus(ctx, "b", TaskRunning, PhaseGate, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "c", TaskBlocked, "", "budget"); err != nil {
		t.Fatal(err)
	}

	got, err = s.TaskSummaries(ctx, "")
	if err != nil || len(got) != 3 {
		t.Fatalf("summaries = %+v, %v", got, err)
	}
	if got[0].ID != "c" || got[1].ID != "a" || got[2].ID != "b" {
		t.Fatalf("order = %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Attempts != 0 || got[0].Usage != (budget.Usage{}) || got[0].Status != TaskBlocked {
		t.Fatalf("no-attempt task = %+v", got[0])
	}
	wantUsage := budget.Usage{InputTokens: 150, OutputTokens: 10, CacheWriteTokens: 5, CacheReadTokens: 900, CostUSD: 0.75}
	if got[2].Attempts != 2 || got[2].Usage != wantUsage || got[2].Status != TaskRunning || got[2].Phase != PhaseGate {
		t.Fatalf("aggregated task = %+v", got[2])
	}

	got, err = s.TaskSummaries(ctx, TaskBlocked)
	if err != nil || len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("blocked filter = %+v, %v", got, err)
	}
	got, err = s.TaskSummaries(ctx, TaskVerified)
	if err != nil || len(got) != 0 {
		t.Fatalf("verified filter = %+v, %v", got, err)
	}
}
