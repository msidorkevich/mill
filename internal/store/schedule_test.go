package store

import (
	"context"
	"slices"
	"testing"
)

func TestScheduleCandidatesListsRunnableGoalsAndPendingSingleTasks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	goal := func(status string, verified ...string) string {
		t.Helper()
		g, err := s.CreateGoal(ctx, status, nil)
		if err != nil {
			t.Fatal(err)
		}
		if status == GoalDraft {
			return g.ID
		}
		if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
			t.Fatal(err)
		}
		for _, local := range verified {
			if err := s.SetTaskStatus(ctx, TaskID(g.ID, local), TaskVerified, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetGoalStatus(ctx, g.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	draft := goal(GoalDraft)
	approved := goal(GoalApproved)
	running := goal(GoalRunning)
	blocked := goal(GoalBlocked)
	doneOpen := goal(GoalDone, "a", "b")
	doneAll := goal(GoalDone, "a", "b", "c")
	landed := goal(GoalLanded, "a", "b", "c")
	for _, id := range []string{"s1", "s2"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTaskStatus(ctx, "s2", TaskBlocked, "", "x"); err != nil {
		t.Fatal(err)
	}

	sch, err := s.ScheduleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var goals []string
	for _, g := range sch.Goals {
		goals = append(goals, g.ID)
	}
	if want := []string{approved, running, blocked, doneOpen}; !slices.Equal(goals, want) {
		t.Fatalf("goals = %v, want %v (not draft %s, done %s, landed %s)", goals, want, draft, doneAll, landed)
	}
	var tasks []string
	for _, tk := range sch.Tasks {
		tasks = append(tasks, tk.ID)
	}
	var want []string
	for _, g := range []string{approved, running, blocked, doneOpen} {
		want = append(want, TaskID(g, "a"), TaskID(g, "b"), TaskID(g, "c"))
	}
	want = append(want, "s1")
	if !slices.Equal(tasks, want) {
		t.Fatalf("tasks = %v, want %v", tasks, want)
	}
	c := TaskID(approved, "c")
	if got := sch.Deps[c]; !slices.Equal(got, []string{TaskID(approved, "b"), TaskID(approved, "a")}) {
		t.Fatalf("deps of %s = %v", c, got)
	}
	if _, ok := sch.Deps[TaskID(doneAll, "c")]; ok {
		t.Fatalf("deps list a task of a finished goal: %v", sch.Deps)
	}
}
