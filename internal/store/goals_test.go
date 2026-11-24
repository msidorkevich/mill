package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func goalPlan() plan.Plan {
	a, b, c := spec("a"), spec("b"), spec("c")
	a.DependsOn = []string{}
	b.DependsOn = []string{"a"}
	c.DependsOn = []string{"b", "a"}
	return plan.Plan{Goal: "g", Assumptions: []string{}, Tasks: []taskspec.Spec{a, b, c}}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return open(t, filepath.Join(t.TempDir(), "mill.db"))
}

func TestMigrationFourUpgradesSchemaThreeInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO goals (id, title, status, created_at, updated_at) VALUES ('g1','old','draft','x','x')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s := open(t, path)
	v, _ := s.SchemaVersion(ctx)
	if v != LatestVersion || LatestVersion < 4 {
		t.Fatalf("version = %d", v)
	}
	var reason, start string
	if err := s.DB().QueryRow(`SELECT reason FROM goals WHERE id='g1'`).Scan(&reason); err != nil {
		t.Fatalf("goals.reason: %v", err)
	}
	if _, err := s.DB().Exec(`SELECT start_sha FROM branches`); err != nil {
		t.Fatalf("branches.start_sha: %v", err)
	}
	_ = start
}

func TestCreateGoalNumbersNumerically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	for i := 1; i <= 10; i++ {
		g, err := s.CreateGoal(ctx, "t", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("g%d", i); g.ID != want {
			t.Fatalf("id = %s, want %s", g.ID, want)
		}
		if g.Status != GoalDraft || !g.CreatedAt.Equal(fixed) {
			t.Fatalf("goal = %+v", g)
		}
	}
	list, err := s.ListGoals(ctx)
	if err != nil || len(list) != 10 || list[0].ID != "g1" || list[9].ID != "g10" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	evs, _ := s.Events(ctx, 0, 0)
	if len(evs) != 10 || evs[0].Kind != "goal-created" || evs[0].GoalID != "g1" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestGoalStatusAndNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", json.RawMessage(`{"x":1}`))
	if err := s.SetGoalStatus(ctx, g.ID, GoalBlocked, "why"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGoal(ctx, g.ID)
	if err != nil || got.Status != GoalBlocked || got.Reason != "why" || string(got.Plan) != `{"x":1}` {
		t.Fatalf("goal = %+v, %v", got, err)
	}
	evs, _ := s.Events(ctx, 0, 0)
	if last := evs[len(evs)-1]; last.Kind != "goal-status" || last.GoalID != g.ID {
		t.Fatalf("last event = %+v", last)
	}
	if _, err := s.GetGoal(ctx, "g99"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGoal err = %v", err)
	}
	if err := s.SetGoalStatus(ctx, "g99", GoalDone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGoalStatus err = %v", err)
	}
}

func TestUpdateGoalPlanOnlyWhileDraft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	if err := s.UpdateGoalPlan(ctx, g.ID, json.RawMessage(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetGoal(ctx, g.ID)
	if string(got.Plan) != `{"v":2}` {
		t.Fatalf("plan = %s", got.Plan)
	}
	evs, _ := s.Events(ctx, 0, 0)
	if evs[len(evs)-1].Kind != "goal-plan-updated" {
		t.Fatalf("events = %+v", evs)
	}
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateGoalPlan(ctx, g.ID, json.RawMessage(`{"v":3}`))
	if err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Fatalf("err = %v", err)
	}
	if err := s.UpdateGoalPlan(ctx, "g99", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing goal err = %v", err)
	}
}

func TestApproveGoalCreatesTasksAndDeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetGoal(ctx, g.ID)
	if got.Status != GoalApproved {
		t.Fatalf("status = %s", got.Status)
	}
	p, err := plan.Parse(got.Plan)
	if err != nil || len(p.Tasks) != 3 {
		t.Fatalf("stored plan = %s, %v", got.Plan, err)
	}
	tasks, err := s.GoalTasks(ctx, g.ID)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	for i, want := range []string{"g1/a", "g1/b", "g1/c"} {
		if tasks[i].ID != want || tasks[i].GoalID != "g1" || tasks[i].Status != TaskPending {
			t.Fatalf("task %d = %+v", i, tasks[i])
		}
	}
	deps, err := s.TaskDeps(ctx, "g1/c")
	if err != nil || !reflect.DeepEqual(deps, []string{"g1/b", "g1/a"}) {
		t.Fatalf("deps = %v, %v", deps, err)
	}
	if deps, _ := s.TaskDeps(ctx, "g1/a"); len(deps) != 0 {
		t.Fatalf("deps of a = %v", deps)
	}
	evs, _ := s.Events(ctx, 0, 0)
	created := 0
	for _, e := range evs {
		if e.Kind == "task-created" && e.TaskID != "" && e.GoalID == "g1" {
			created++
		}
	}
	if created != 3 {
		t.Fatalf("task-created events = %d", created)
	}
}

func TestApproveGoalFailureLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	// A task with the id the plan's second task will get already exists.
	clash := spec("g1/b")
	if err := s.CreateTask(ctx, clash); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Events(ctx, 0, 0)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err == nil {
		t.Fatal("approve succeeded despite id clash")
	}
	got, _ := s.GetGoal(ctx, g.ID)
	if got.Status != GoalDraft {
		t.Fatalf("status = %s", got.Status)
	}
	if tasks, _ := s.GoalTasks(ctx, g.ID); len(tasks) != 0 {
		t.Fatalf("tasks left behind: %+v", tasks)
	}
	var n int
	s.DB().QueryRow(`SELECT COUNT(*) FROM deps`).Scan(&n)
	if n != 0 {
		t.Fatalf("deps left behind: %d", n)
	}
	if _, err := s.GetTask(ctx, "g1/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("g1/a err = %v", err)
	}
	after, _ := s.Events(ctx, 0, 0)
	if len(after) != len(before) {
		t.Fatalf("events written by failed approve: %d", len(after)-len(before))
	}
}

func TestApproveGoalRefusesNonDraft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err == nil {
		t.Fatal("second approve accepted")
	}
	if err := s.ApproveGoal(ctx, "g99", goalPlan()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing goal err = %v", err)
	}
}

func TestGoalUsageSumsAttemptsAcrossTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "t", nil)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec("other")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1/a", "g1/b", "other"} {
		aid, err := s.StartAttempt(ctx, id, 1, "implementer")
		if err != nil {
			t.Fatal(err)
		}
		u := budget.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5}
		if err := s.FinishAttempt(ctx, aid, "ok", u, false, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.GoalUsage(ctx, "g1")
	if err != nil || u.InputTokens != 20 || u.OutputTokens != 10 || u.CostUSD != 1.0 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}

func TestTaskIDRoundTrip(t *testing.T) {
	t.Parallel()
	id := TaskID("g1", "a")
	if id != "g1/a" {
		t.Fatalf("id = %s", id)
	}
	if g, l := SplitTaskID(id); g != "g1" || l != "a" {
		t.Fatalf("split = %q %q", g, l)
	}
	if g, l := SplitTaskID("plain"); g != "" || l != "plain" {
		t.Fatalf("split plain = %q %q", g, l)
	}
}

func TestBranchStartSHASavedAndLoaded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateTask(ctx, spec("t1")); err != nil {
		t.Fatal(err)
	}
	b := Branch{TaskID: "t1", Name: "mill/t1", Base: "main", BaseSHA: "aaa", StartSHA: "bbb"}
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBranch(ctx, "t1")
	if err != nil || got.StartSHA != "bbb" {
		t.Fatalf("branch = %+v, %v", got, err)
	}
	b.StartSHA = "ccc"
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBranch(ctx, "t1"); got.StartSHA != "ccc" {
		t.Fatalf("updated branch = %+v", got)
	}
}

func TestResetGoalTasksMatchesStatusAndReason(t *testing.T) {
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
	a, b, c := TaskID(g.ID, "a"), TaskID(g.ID, "b"), TaskID(g.ID, "c")
	for _, x := range []struct{ id, status, reason string }{
		{a, TaskBlocked, "interrupted"}, {b, TaskFailed, "mill error: boom"}, {c, TaskBlocked, "gate failed"},
	} {
		if err := s.SetTaskStatus(ctx, x.id, x.status, PhaseGate, x.reason); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := s.ResetGoalTasks(ctx, g.ID, []string{TaskBlocked}, "interrupted", "interrupted")
	if err != nil || !reflect.DeepEqual(ids, []string{a}) {
		t.Fatalf("interrupted reset = %v, %v", ids, err)
	}
	if got, _ := s.GetTask(ctx, a); got.Status != TaskPending || got.Phase != "" || got.Reason != "interrupted" {
		t.Fatalf("a = %+v", got)
	}
	if got, _ := s.GetTask(ctx, c); got.Status != TaskBlocked {
		t.Fatalf("c should stay blocked: %+v", got)
	}

	ids, err = s.ResetGoalTasks(ctx, g.ID, []string{TaskBlocked, TaskFailed}, "", "retry requested")
	if err != nil || !reflect.DeepEqual(ids, []string{b, c}) {
		t.Fatalf("retry reset = %v, %v", ids, err)
	}
	for _, id := range []string{b, c} {
		if got, _ := s.GetTask(ctx, id); got.Status != TaskPending || got.Reason != "retry requested" {
			t.Fatalf("%s = %+v", id, got)
		}
	}
	evs, err := s.TaskEvents(ctx, c)
	if err != nil || evs[len(evs)-1].Kind != "task-status" || !strings.Contains(string(evs[len(evs)-1].Data), "retry requested") {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}

func TestUpdateGoalTaskSpecChangesRowPlanAndRecordsEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, _ := s.CreateGoal(ctx, "g", nil)
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	upd := spec("b")
	upd.Spec = "new body"
	upd.DependsOn = []string{"zzz"} // ignored: dependencies are fixed
	id := TaskID(g.ID, "b")
	if err := s.UpdateGoalTaskSpec(ctx, id, upd); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.GetTask(ctx, id)
	if tk.Spec.Spec != "new body" {
		t.Errorf("task = %+v", tk)
	}
	got, _ := s.GetGoal(ctx, g.ID)
	p, err := plan.Parse(got.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tasks[1].Spec != "new body" || !reflect.DeepEqual(p.Tasks[1].DependsOn, []string{"a"}) || p.Tasks[0].Spec == "new body" {
		t.Errorf("plan = %+v", p.Tasks)
	}
	evs, _ := s.TaskEvents(ctx, id)
	if last := evs[len(evs)-1]; last.Kind != "task-spec-updated" {
		t.Errorf("events = %+v", evs)
	}
	if err := s.UpdateGoalTaskSpec(ctx, TaskID(g.ID, "nope"), upd); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}
