package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/taskspec"
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
	if err := s.FinishAttempt(ctx, a1, "ok", budget.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.5}, false, "out.json", "log"); err != nil {
		t.Fatal(err)
	}
	a2, _ := s.StartAttempt(ctx, "t1", 1, "verifier")
	if err := s.FinishAttempt(ctx, a2, "ok", budget.Usage{InputTokens: 50, CacheReadTokens: 7, CostUSD: 0.25}, false, "", ""); err != nil {
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
	s.FinishAttempt(ctx, a1, "ok", budget.Usage{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 5, CacheReadTokens: 900, CostUSD: 0.5}, false, "", "")
	a2, _ := s.StartAttempt(ctx, "b", 2, "implementer")
	s.FinishAttempt(ctx, a2, "ok", budget.Usage{InputTokens: 50, CostUSD: 0.25}, false, "", "")
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
	must(s.FinishAttempt(ctx, fin, "ok", budget.Usage{}, false, "", ""))

	tasks, attempts, err := s.RecoverInterrupted(ctx, nil)
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
	if tasks, attempts, err := s.RecoverInterrupted(ctx, nil); err != nil || tasks != 0 || attempts != 0 {
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
	if err := s.FinishAttempt(ctx, a2, "ok", budget.Usage{}, false, "", ""); err != nil {
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
	// CreateTask writes columns this old schema lacks: insert the row the way it was then.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks (id, title, kind, spec, status, created_at, updated_at)
		VALUES ('t1', 'T', 'implement', 's', 'pending', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	old, err := s.StartAttempt(ctx, "t1", 1, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion {
		t.Fatalf("schema = %d, want %d", v, LatestVersion)
	}
	got, err := s2.RunningAttempts(ctx)
	if err != nil || len(got) != 1 || got[0].ID != old || got[0].PID != 0 || got[0].Command != "" {
		t.Fatalf("running after upgrade = %+v, %v", got, err)
	}
	if err := s2.SetAttemptProcess(ctx, old, 7, "claude", "log"); err != nil {
		t.Fatal(err)
	}
}

func TestFinishAttemptRecordsCostEstimated(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	a1, _ := s.StartAttempt(ctx, "t1", 1, "implementer")
	a2, _ := s.StartAttempt(ctx, "t1", 1, "verifier")
	if err := s.FinishAttempt(ctx, a1, "error", budget.Usage{InputTokens: 1, CostUSD: 0.02}, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, a2, "ok", budget.Usage{InputTokens: 1, CostUSD: 0.02}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.Attempts(ctx, "t1")
	if err != nil || len(list) != 2 || !list[0].CostEstimated || list[1].CostEstimated {
		t.Fatalf("attempts = %+v, %v", list, err)
	}
}

// A database at schema v5 upgrades and gains attempts.cost_estimated.
func TestMigrateFromV5AddsCostEstimated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:5]
	LatestVersion = 5
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion || v < 6 {
		t.Fatalf("schema = %d, want %d", v, LatestVersion)
	}
	if err := s2.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	a, _ := s2.StartAttempt(ctx, "t1", 1, "implementer")
	if err := s2.FinishAttempt(ctx, a, "ok", budget.Usage{}, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := s2.Attempts(ctx, "t1"); len(list) != 1 || !list[0].CostEstimated {
		t.Fatalf("attempts = %+v", list)
	}
}

func TestRecoverInterruptedRecordsUsageOfAbandonedAttempts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	withStream, err := s.StartAttempt(ctx, "g1/a", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	noStream, err := s.StartAttempt(ctx, "g1/b", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	u := budget.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 7, CostUSD: 0.25}
	if _, attempts, err := s.RecoverInterrupted(ctx, map[int64]AbandonedUsage{withStream: {Usage: u, CostEstimated: true}}); err != nil || attempts != 2 {
		t.Fatalf("RecoverInterrupted = %d, %v", attempts, err)
	}
	a, _ := s.Attempts(ctx, "g1/a")
	if a[0].Status != AttemptAbandoned || a[0].Usage != u || !a[0].CostEstimated {
		t.Errorf("attempt with stream = %+v", a[0])
	}
	b, _ := s.Attempts(ctx, "g1/b")
	if b[0].Status != AttemptAbandoned || b[0].Usage != (budget.Usage{}) || b[0].CostEstimated {
		t.Errorf("attempt without stream = %+v", b[0])
	}
	_ = noStream
	if got, err := s.TaskUsage(ctx, "g1/a"); err != nil || got != u {
		t.Errorf("TaskUsage = %+v, %v; want %+v", got, err, u)
	}
	if got, err := s.GoalUsage(ctx, g.ID); err != nil || got != u {
		t.Errorf("GoalUsage = %+v, %v; want %+v", got, err, u)
	}
}

func TestBranchRoundTripsPublishedAndGateSHA(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	b := Branch{TaskID: "t1", Name: "mill/t1", Base: "main", BaseSHA: "b", HeadSHA: "h", Worktree: "/w",
		PublishedRef: "review/t1", PublishedSHA: "p1", GateSHA: "g1"}
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBranch(ctx, "t1")
	if err != nil || got.PublishedSHA != "p1" || got.GateSHA != "g1" || got.PublishedRef != "review/t1" {
		t.Fatalf("GetBranch = %+v, %v", got, err)
	}
	b.PublishedSHA, b.GateSHA = "p2", "g2"
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBranch(ctx, "t1"); got.PublishedSHA != "p2" || got.GateSHA != "g2" {
		t.Fatalf("after update = %+v", got)
	}
}

func TestMarkTaskCleanedClearsWorktreeKeepsRowAndRecordsEvent(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, Branch{TaskID: "t1", Name: "mill/t1", Base: "main", Worktree: "/w", PublishedSHA: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTaskCleaned(ctx, "t1", map[string]string{"branch": "mill/t1"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBranch(ctx, "t1")
	if err != nil || got.Worktree != "" || got.Name != "mill/t1" || got.PublishedSHA != "p1" {
		t.Fatalf("GetBranch = %+v, %v", got, err)
	}
	evs, _ := s.TaskEvents(ctx, "t1")
	if last := evs[len(evs)-1]; last.Kind != "task-cleaned" {
		t.Errorf("last event = %q, want task-cleaned", last.Kind)
	}
	if err := s.MarkTaskCleaned(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkTaskCleaned(unknown) = %v, want ErrNotFound", err)
	}
}

// A database at schema v6 upgrades; existing branches gain empty published_sha and gate_sha.
func TestMigrateFromV6AddsBranchPublishAndGateSHA(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:6]
	LatestVersion = 6
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	// CreateTask writes columns this old schema lacks: insert the row the way it was then.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks (id, title, kind, spec, status, created_at, updated_at)
		VALUES ('t1', 'T', 'implement', 's', 'pending', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO branches (task_id, name, base, base_sha, head_sha, updated_at)
		VALUES ('t1', 'mill/t1', 'main', 'b', 'h', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion || v < 7 {
		t.Fatalf("schema = %d, want %d", v, LatestVersion)
	}
	got, err := s2.GetBranch(ctx, "t1")
	if err != nil || got.HeadSHA != "h" || got.PublishedSHA != "" || got.GateSHA != "" {
		t.Fatalf("branch after upgrade = %+v, %v", got, err)
	}
	got.GateSHA = "g"
	if err := s2.SaveBranch(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.GetBranch(ctx, "t1"); got.GateSHA != "g" {
		t.Fatalf("gate_sha = %q", got.GateSHA)
	}
}

func TestDoneMeansVerifiedPublishedOrLanded(t *testing.T) {
	for _, st := range TaskStatuses {
		want := st == TaskVerified || st == TaskPublished || st == TaskLanded
		if Done(st) != want {
			t.Errorf("Done(%q) = %v, want %v", st, !want, want)
		}
	}
}

func TestTaskDescendantsAreTransitiveAndRejectHoldsNonLanded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []string{"g/a", "g/b", "g/c", "g/d", "g/x"} {
		if err := s.CreateTask(ctx, taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "s", Size: "S"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range [][2]string{{"g/b", "g/a"}, {"g/c", "g/b"}, {"g/d", "g/a"}, {"g/d", "g/c"}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO deps (task_id, depends_on) VALUES (?, ?)`, d[0], d[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TaskDescendants(ctx, "g/a")
	if err != nil || len(got) != 3 || got[0] != "g/b" || got[1] != "g/d" || got[2] != "g/c" {
		t.Fatalf("descendants = %v, %v", got, err)
	}
	if err := s.SetTaskStatus(ctx, "g/c", TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	held, err := s.RejectTask(ctx, "g/a", "rejected: no")
	if err != nil || len(held) != 2 {
		t.Fatalf("held = %v, %v", held, err)
	}
	for id, want := range map[string]string{"g/a": TaskBlocked, "g/b": TaskBlocked, "g/c": TaskLanded, "g/d": TaskBlocked, "g/x": TaskPending} {
		if tk, _ := s.GetTask(ctx, id); tk.Status != want {
			t.Errorf("%s = %s, want %s", id, tk.Status, want)
		}
	}
}

func TestUpdateAttemptUsageOnlyWhileRunning(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "t1", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	live := budget.Usage{InputTokens: 10, OutputTokens: 2, CacheWriteTokens: 3, CacheReadTokens: 4, CostUSD: 0.01}
	if err := s.UpdateAttemptUsage(ctx, id, live, true); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Attempts(ctx, "t1")
	if list[0].Usage != live || !list[0].CostEstimated {
		t.Fatalf("running attempt = %+v", list[0])
	}
	final := budget.Usage{InputTokens: 100, CostUSD: 0.5}
	if err := s.FinishAttempt(ctx, id, "ok", final, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAttemptUsage(ctx, id, budget.Usage{InputTokens: 1}, true); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Attempts(ctx, "t1")
	if list[0].Usage != final || list[0].CostEstimated {
		t.Fatalf("finished attempt changed: %+v", list[0])
	}
}

func TestSetTaskReasonKeepsStatusAndPhase(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "t1", TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskReason(ctx, "t1", "conflicts with t2"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskVerified || got.Phase != "" || got.Reason != "conflicts with t2" {
		t.Fatalf("task = %+v", got)
	}
	if err := s.SetTaskReason(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: err = %v, want ErrNotFound", err)
	}
}

func TestRunningStagesListsRunningTasksWithoutRunningAttempt(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	for _, id := range []string{"gating", "coding", "idle"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.StartAttempt(ctx, "gating", 3, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, a, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "gating", TaskRunning, PhaseGate, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "coding", TaskRunning, PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "coding", 1, "implementer"); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunningStages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "gating" || got[0].Phase != PhaseGate || got[0].N != 3 || got[0].Since.IsZero() {
		t.Fatalf("stages = %+v", got)
	}
}

func TestBlockRestackRecordsFormerStatusUntilTheNextStatusChange(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "t1", TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveTask(ctx, "t1", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.BlockRestack(ctx, "t1", TaskPending, "x"); err == nil {
		t.Fatal("restack block of a pending task accepted")
	}
	if err := s.BlockRestack(ctx, "t1", TaskVerified, "restack conflicts"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, "t1")
	if got.Status != TaskBlocked || got.Reason != "restack conflicts" || got.RestackBlockedFrom != TaskVerified || !got.Approved {
		t.Fatalf("task = %+v", got)
	}
	// Any other block replaces it.
	if err := s.SetTaskStatus(ctx, "t1", TaskBlocked, "", "rejected"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.RestackBlockedFrom != "" || got.Reason != "rejected" {
		t.Fatalf("task = %+v", got)
	}
	if err := s.BlockRestack(ctx, "missing", TaskVerified, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task err = %v", err)
	}
}

func TestTaskBlockedByOlderRestackConflictCountsAsRestackBlocked(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	for _, id := range []string{"v", "p", "k", "other"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveBranch(ctx, Branch{TaskID: "p", Name: "mill/t/p", PublishedRef: "review/p", PublishedSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	// Blocks recorded the way sync did before it kept the former status; a keeper reason is
	// ambiguous (it also covers failures after a resolution) and is not a restack block.
	for id, reason := range map[string]string{
		"v":     "sync: restacking onto origin/main (1234abcd) conflicts in b.txt; the branch is unchanged",
		"p":     "sync: restacking fan-in task onto origin/main (1234abcd) conflicts at replaying x in b.txt; the branch is unchanged. Restack it by hand",
		"k":     "keeper: rebasing onto origin/main (1234abcd) failed at verify: fail; the branch is unchanged",
		"other": "sync: the gate failed after restacking onto origin/main",
	} {
		if err := s.SetTaskStatus(ctx, id, TaskBlocked, "", reason); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"v": TaskVerified, "p": TaskPublished, "k": "", "other": ""} {
		if got, err := s.GetTask(ctx, id); err != nil || got.RestackBlockedFrom != want {
			t.Errorf("%s: blocked from %q (err %v), want %q", id, got.RestackBlockedFrom, err, want)
		}
	}
}
