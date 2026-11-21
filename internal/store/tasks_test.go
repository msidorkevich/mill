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

func TestTimestampsFixedWidthAndOrdered(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))

	// A whole second sorts after ".1Z" under RFC3339Nano text order; fixed width must not.
	s.now = func() time.Time { return fixed }
	if err := s.CreateTask(ctx, spec("whole")); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixed.Add(100 * time.Millisecond) }
	if err := s.CreateTask(ctx, spec("frac")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "x", "", "", nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.TaskSummaries(ctx, "")
	if err != nil || len(got) != 2 || got[0].ID != "whole" || got[1].ID != "frac" {
		t.Fatalf("TaskSummaries = %+v, %v; want whole then frac", got, err)
	}
	for _, q := range []string{"SELECT created_at FROM tasks", "SELECT updated_at FROM tasks", "SELECT at FROM events"} {
		rows, err := s.DB().QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			rows.Scan(&v)
			if len(v) != len("2006-01-02T15:04:05.000000000Z") || v[19] != '.' || v[len(v)-1] != 'Z' {
				t.Errorf("%s: %q is not fixed width", q, v)
			}
		}
		rows.Close()
	}
}

func TestMigrateV2NormalizesTimestamps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:2]
	LatestVersion = 2
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	old := []string{
		"2026-11-21T12:00:00Z", "2026-11-21T12:00:00.1Z", "2026-11-21T12:00:00.12Z",
		"2026-11-21T12:00:00.123456Z", "2026-11-21T12:00:00.123456789Z",
	}
	db := s.DB()
	for i, v := range old {
		id := "t" + string(rune('a'+i))
		if _, err := db.ExecContext(ctx, `INSERT INTO goals (id, title, status, created_at, updated_at) VALUES (?, 'g', 's', ?, ?)`, id, v, v); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, title, kind, spec, status, created_at, updated_at) VALUES (?, 't', 'implement', 's', 'pending', ?, ?)`, id, v, v); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO attempts (task_id, n, role, status, started_at, ended_at) VALUES (?, 1, 'implementer', 'ok', ?, ?)`, id, v, v); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO branches (task_id, name, base, base_sha, updated_at) VALUES (?, ?, 'main', 'sha', ?)`, id, "b"+id, v); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO events (at, kind) VALUES (?, 'x')`, v); err != nil {
			t.Fatal(err)
		}
	}
	// An attempt that never ended keeps a NULL ended_at.
	if _, err := db.ExecContext(ctx, `INSERT INTO attempts (task_id, n, role, status, started_at) VALUES ('ta', 2, 'implementer', 'running', ?)`, old[0]); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	db = s2.DB()
	for _, c := range []struct{ table, col string }{
		{"goals", "created_at"}, {"goals", "updated_at"}, {"tasks", "created_at"}, {"tasks", "updated_at"},
		{"attempts", "started_at"}, {"attempts", "ended_at"}, {"branches", "updated_at"},
	} {
		rows, err := db.QueryContext(ctx, "SELECT id_, v FROM (SELECT rowid AS id_, "+c.col+" AS v FROM "+c.table+") WHERE v IS NOT NULL ORDER BY id_")
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for rows.Next() {
			var id int
			var v string
			rows.Scan(&id, &v)
			want, _ := time.Parse(time.RFC3339Nano, old[i%len(old)])
			if len(v) != 30 || v[19] != '.' || v[29] != 'Z' {
				t.Errorf("%s.%s: %q not normalized", c.table, c.col, v)
			}
			if got, err := parseTime(v); err != nil || !got.Equal(want) {
				t.Errorf("%s.%s: %q parses to %v, %v; want %v", c.table, c.col, v, got, err, want)
			}
			i++
		}
		rows.Close()
	}
	var nulls int
	db.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE ended_at IS NULL`).Scan(&nulls)
	if nulls != 1 {
		t.Errorf("NULL ended_at count = %d, want 1", nulls)
	}
	rows, _ := db.QueryContext(ctx, `SELECT at FROM events ORDER BY seq`)
	defer rows.Close()
	for i := 0; rows.Next(); i++ {
		var v string
		rows.Scan(&v)
		if v != old[i] {
			t.Errorf("event %d at = %q, want untouched %q", i, v, old[i])
		}
	}
}

func TestRecoverInterruptedResetsOnlyRunningWork(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	for _, id := range []string{"run1", "run2", "done", "idle"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetTaskStatus(ctx, "run1", TaskRunning, PhaseGate, ""))
	must(s.SetTaskStatus(ctx, "run2", TaskRunning, PhaseImplement, ""))
	must(s.SetTaskStatus(ctx, "done", TaskVerified, "", ""))
	if _, err := s.StartAttempt(ctx, "run1", 1, "implementer"); err != nil {
		t.Fatal(err)
	}
	fin, _ := s.StartAttempt(ctx, "done", 1, "implementer")
	must(s.FinishAttempt(ctx, fin, "ok", budget.Usage{}, "", ""))

	tasks, attempts, err := s.RecoverInterrupted(ctx)
	if err != nil || tasks != 2 || attempts != 1 {
		t.Fatalf("RecoverInterrupted = %d, %d, %v; want 2, 1, nil", tasks, attempts, err)
	}
	for id, want := range map[string]string{"run1": TaskPending, "run2": TaskPending, "done": TaskVerified, "idle": TaskPending} {
		got, _ := s.GetTask(ctx, id)
		if got.Status != want {
			t.Errorf("%s status = %s, want %s", id, got.Status, want)
		}
	}
	got, _ := s.GetTask(ctx, "run1")
	if got.Phase != "" || got.Reason != "recovered after an interrupted run" {
		t.Errorf("run1 phase %q reason %q", got.Phase, got.Reason)
	}
	atts, _ := s.Attempts(ctx, "run1")
	if len(atts) != 1 || atts[0].Status != "abandoned" || atts[0].EndedAt == nil {
		t.Errorf("run1 attempts = %+v", atts)
	}
	atts, _ = s.Attempts(ctx, "done")
	if atts[0].Status != "ok" {
		t.Errorf("finished attempt touched: %+v", atts)
	}
	evs, _ := s.TaskEvents(ctx, "run2")
	found := false
	for _, e := range evs {
		found = found || e.Kind == "task-recovered"
	}
	if !found {
		t.Error("no task-recovered event for run2")
	}
	if tasks, attempts, err := s.RecoverInterrupted(ctx); err != nil || tasks != 0 || attempts != 0 {
		t.Fatalf("second RecoverInterrupted = %d, %d, %v; want 0, 0, nil", tasks, attempts, err)
	}
}

func TestAttemptProcessRecordedAndListed(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	a1, _ := s.StartAttempt(ctx, "t1", 1, "implementer")
	a2, _ := s.StartAttempt(ctx, "t1", 1, "verifier")
	if err := s.SetAttemptProcess(ctx, a1, 4242, "claude", "/tmp/raw.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, a2, "ok", budget.Usage{}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, a2, 1, "claude", "x"); err == nil {
		t.Fatal("SetAttemptProcess accepted a finished attempt")
	}
	got, err := s.RunningAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a1 || got[0].PID != 4242 || got[0].Command != "claude" || got[0].LogPath != "/tmp/raw.jsonl" {
		t.Fatalf("running = %+v", got)
	}
}

func TestMigrateV4AddsAttemptProcessColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:4]
	LatestVersion = 4
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	old, err := s.StartAttempt(ctx, "t1", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != 5 {
		t.Fatalf("schema = %d, want 5", v)
	}
	got, err := s2.RunningAttempts(ctx)
	if err != nil || len(got) != 1 || got[0].ID != old || got[0].PID != 0 || got[0].Command != "" {
		t.Fatalf("running after upgrade = %+v, %v", got, err)
	}
	if err := s2.SetAttemptProcess(ctx, old, 7, "claude", "log"); err != nil {
		t.Fatal(err)
	}
}
