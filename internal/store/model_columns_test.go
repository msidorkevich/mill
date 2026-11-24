package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateFromV9OldTasksReadMediumComplexityAndNoTaskModel(t *testing.T) {
	// Serial: swaps the package-level migrations and LatestVersion that every Open reads.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:9]
	LatestVersion = 9
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks (id, title, kind, spec, status, created_at, updated_at)
		VALUES ('t1', 'T', 'implement', 's', 'pending', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO attempts (task_id, n, role, status, started_at)
		VALUES ('t1', 1, 'implementer', 'ok', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion || v < 10 {
		t.Fatalf("schema = %d, want %d (>= 10)", v, LatestVersion)
	}
	got, err := s2.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Complexity != "medium" || got.ComplexityReason != "" || got.Model != "" || got.Title != "T" {
		t.Fatalf("task after upgrade = %+v", got)
	}
	as, err := s2.Attempts(ctx, "t1")
	if err != nil || len(as) != 1 || as[0].Model != "" || as[0].ModelReason != "" {
		t.Fatalf("attempts after upgrade = %+v, %v", as, err)
	}
}

func TestTaskModelAndComplexityRoundTripThroughCreateTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	sp := spec("t1")
	sp.Complexity = "high"
	sp.ComplexityReason = "touches the scheduler and the store"
	sp.Model = "opus"
	if err := s.CreateTask(ctx, sp); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Complexity != "high" || got.ComplexityReason != sp.ComplexityReason || got.Model != "opus" {
		t.Fatalf("task = %+v", got)
	}
}

func TestTaskWithEmptyComplexityReadsMedium(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil || got.Complexity != "medium" {
		t.Fatalf("task = %+v, %v", got, err)
	}
}

func TestGoalTasksCarryComplexityAndTaskModelFromPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, err := s.CreateGoal(ctx, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := goalPlan()
	p.Tasks[0].Complexity = "low"
	p.Tasks[0].ComplexityReason = "one-line change"
	p.Tasks[1].Complexity = "high"
	p.Tasks[1].ComplexityReason = "new protocol"
	p.Tasks[1].Model = "opus"
	if err := s.ApproveGoal(ctx, g.ID, p); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.GoalTasks(ctx, g.ID)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	want := []struct{ c, r, m string }{
		{"low", "one-line change", ""},
		{"high", "new protocol", "opus"},
		{"medium", "", ""},
	}
	for i, w := range want {
		got := tasks[i]
		if got.Complexity != w.c || got.ComplexityReason != w.r || got.Model != w.m {
			t.Fatalf("task %s = complexity %q reason %q model %q, want %+v",
				got.ID, got.Complexity, got.ComplexityReason, got.Model, w)
		}
	}
}

func TestUpdateTaskSpecPersistsTaskModelAndComplexity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	sp := spec("t1")
	sp.Complexity = "low"
	sp.Model = "haiku"
	if err := s.CreateTask(ctx, sp); err != nil {
		t.Fatal(err)
	}
	sp.Complexity = "high"
	sp.ComplexityReason = "harder than it looked"
	sp.Model = "opus"
	if err := s.UpdateTaskSpec(ctx, sp); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Complexity != "high" || got.ComplexityReason != "harder than it looked" || got.Model != "opus" {
		t.Fatalf("task = %+v", got)
	}
}

func TestUpdateGoalTaskSpecPersistsTaskModelAndComplexity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, err := s.CreateGoal(ctx, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	sp := spec("b")
	sp.Complexity = "high"
	sp.ComplexityReason = "needs a migration"
	sp.Model = "opus"
	id := TaskID(g.ID, "b")
	if err := s.UpdateGoalTaskSpec(ctx, id, sp); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complexity != "high" || got.ComplexityReason != "needs a migration" || got.Model != "opus" {
		t.Fatalf("task = %+v", got)
	}
	goal, err := s.GetGoal(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goal.Plan), `"model":"opus"`) {
		t.Fatalf("stored plan lacks the new model: %s", goal.Plan)
	}
}

func TestSetAttemptModelIsReturnedByAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "t1", 1, RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptModel(ctx, id, "opus", "complexity high"); err != nil {
		t.Fatal(err)
	}
	as, err := s.Attempts(ctx, "t1")
	if err != nil || len(as) != 1 {
		t.Fatalf("attempts = %+v, %v", as, err)
	}
	if as[0].Model != "opus" || as[0].ModelReason != "complexity high" {
		t.Fatalf("attempt = %+v", as[0])
	}
}

func TestSetAttemptModelUnknownAttemptIsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	err := s.SetAttemptModel(ctx, 4242, "opus", "x")
	if err == nil {
		t.Fatal("SetAttemptModel on a missing attempt succeeded")
	}
	if !strings.Contains(err.Error(), "4242") || !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want it to name attempt 4242 and wrap ErrNotFound", err)
	}
}
