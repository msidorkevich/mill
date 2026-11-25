package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/plan"
)

// depsGoal approves a goal with a, b <- a, c <- b, a and an independent d.
func depsGoal(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	p := goalPlan()
	d := spec("d")
	d.DependsOn = []string{}
	p.Tasks = append(p.Tasks, d)
	g, err := s.CreateGoal(ctx, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, p); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

type depsState struct {
	deps   map[string][]string
	plan   string
	events int
}

func snapshotDeps(t *testing.T, s *Store, goalID string) depsState {
	t.Helper()
	ctx := context.Background()
	st := depsState{deps: map[string][]string{}}
	for _, l := range []string{"a", "b", "c", "d"} {
		ds, err := s.TaskDeps(ctx, TaskID(goalID, l))
		if err != nil {
			t.Fatal(err)
		}
		st.deps[l] = ds
	}
	g, err := s.GetGoal(ctx, goalID)
	if err != nil {
		t.Fatal(err)
	}
	st.plan = string(g.Plan)
	evs, err := s.Events(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.events = len(evs)
	return st
}

func TestAddTaskDepsAppendsRowsPlanAndEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g := depsGoal(t, s)
	id := TaskID(g, "c")
	if err := s.AddTaskDeps(ctx, id, []string{TaskID(g, "d")}); err != nil {
		t.Fatal(err)
	}
	deps, err := s.TaskDeps(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{TaskID(g, "b"), TaskID(g, "a"), TaskID(g, "d")}; !reflect.DeepEqual(deps, want) {
		t.Errorf("deps = %v, want %v", deps, want)
	}
	goal, _ := s.GetGoal(ctx, g)
	p, err := plan.Parse(goal.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Tasks[2].DependsOn; !reflect.DeepEqual(got, []string{"b", "a", "d"}) {
		t.Errorf("plan depends_on of c = %v", got)
	}
	if got := p.Tasks[1].DependsOn; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("plan depends_on of b = %v", got)
	}
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Kind != "task-deps-added" || last.GoalID != g {
		t.Fatalf("last event = %+v", last)
	}
	var data struct {
		Added     []string `json:"added"`
		DependsOn []string `json:"depends_on"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data.Added, []string{"d"}) || !reflect.DeepEqual(data.DependsOn, []string{"b", "a", "d"}) {
		t.Errorf("event data = %s", last.Data)
	}
}

func TestAddTaskDepsRefusesInvalidAndChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g := depsGoal(t, s)
	other := depsGoal(t, s)
	if err := s.SetTaskStatus(ctx, TaskID(g, "b"), TaskRunning, PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, TaskID(g, "d"), TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		task     string
		add      []string
		want     string
		notFound bool
	}{
		{"cycle", TaskID(g, "a"), []string{TaskID(g, "c")}, "cycle: a -> c -> b -> a", false},
		{"unknown", TaskID(g, "c"), []string{TaskID(g, "zzz")}, TaskID(g, "zzz"), true},
		{"other goal", TaskID(g, "c"), []string{TaskID(other, "d")}, TaskID(other, "d"), true},
		{"self", TaskID(g, "c"), []string{TaskID(g, "c")}, "itself", false},
		{"duplicate of existing", TaskID(g, "c"), []string{TaskID(g, "a")}, "already depends on " + TaskID(g, "a"), false},
		{"duplicate in request", TaskID(g, "a"), []string{TaskID(g, "d"), TaskID(g, "d")}, "already depends on", false},
		{"running", TaskID(g, "b"), []string{TaskID(g, "d")}, "is running", false},
		{"verified", TaskID(g, "d"), []string{TaskID(g, "a")}, "is verified", false},
		{"not a goal task", "loose", []string{TaskID(g, "a")}, "not a goal task", false},
	} {
		before := snapshotDeps(t, s, g)
		err := s.AddTaskDeps(ctx, tc.task, tc.add)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
		if tc.notFound && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", tc.name, err)
		}
		if after := snapshotDeps(t, s, g); !reflect.DeepEqual(before, after) {
			t.Errorf("%s: state changed:\nbefore %+v\nafter  %+v", tc.name, before, after)
		}
	}
}

func TestAddTaskDepsAllowsBlockedAndFailedTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g := depsGoal(t, s)
	for _, st := range []string{TaskBlocked, TaskFailed} {
		id := TaskID(g, "d")
		if st == TaskFailed {
			id = TaskID(g, "b")
		}
		if err := s.SetTaskStatus(ctx, id, st, "", "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, TaskID(g, "d"), []string{TaskID(g, "a")}); err != nil {
		t.Errorf("blocked: %v", err)
	}
	if err := s.AddTaskDeps(ctx, TaskID(g, "b"), []string{TaskID(g, "d")}); err != nil {
		t.Errorf("failed: %v", err)
	}
	deps, _ := s.TaskDeps(ctx, TaskID(g, "b"))
	if want := []string{TaskID(g, "a"), TaskID(g, "d")}; !reflect.DeepEqual(deps, want) {
		t.Errorf("deps of b = %v, want %v", deps, want)
	}
}
