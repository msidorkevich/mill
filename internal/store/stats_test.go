package store

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
)

type statsModelsAttempt struct {
	n     int
	role  string
	model string
	cost  float64
}

// statsModelsTask creates a task with the given size, complexity and final status and records
// its attempts in order.
func statsModelsTask(t *testing.T, s *Store, id, size, complexity, status string, attempts ...statsModelsAttempt) {
	t.Helper()
	ctx := context.Background()
	sp := spec(id)
	sp.Size = size
	sp.Complexity = complexity
	if err := s.CreateTask(ctx, sp); err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		aid, err := s.StartAttempt(ctx, id, a.n, a.role)
		if err != nil {
			t.Fatal(err)
		}
		if a.model != "" {
			if err := s.SetAttemptModel(ctx, aid, a.model, "test"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{CostUSD: a.cost}, false, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTaskStatus(ctx, id, status, "", ""); err != nil {
		t.Fatal(err)
	}
}

func statsModelsNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestModelStatsGroupsBySizeComplexityAndFirstAttemptModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	impl, ver := RoleImplementer, "verifier"

	// S/low/sonnet: two tasks. a passes first try; b needs a second implementer attempt
	// (whose model differs: the first attempt's model decides the group).
	statsModelsTask(t, s, "a", "S", "low", TaskVerified,
		statsModelsAttempt{1, impl, "sonnet", 1.0}, statsModelsAttempt{1, ver, "haiku", 0.5})
	statsModelsTask(t, s, "b", "S", "low", TaskLanded,
		statsModelsAttempt{1, impl, "sonnet", 2.0}, statsModelsAttempt{2, impl, "opus", 3.0})
	// S/high/opus: published on the first attempt.
	statsModelsTask(t, s, "c", "S", "high", TaskPublished, statsModelsAttempt{1, impl, "opus", 4.0})
	// M/medium/-: model unrecorded, blocked after one attempt (not a pass).
	statsModelsTask(t, s, "d", "M", "medium", TaskBlocked, statsModelsAttempt{1, impl, "", 0.25})
	// M/low/sonnet: verified, with a fixer attempt that counts toward cost only.
	statsModelsTask(t, s, "e", "M", "low", TaskVerified,
		statsModelsAttempt{1, impl, "sonnet", 1.0}, statsModelsAttempt{2, RoleFixer, "sonnet", 1.0})
	// No implementer attempt: left out.
	statsModelsTask(t, s, "f", "S", "low", TaskPending)
	statsModelsTask(t, s, "g", "S", "low", TaskPending, statsModelsAttempt{1, ver, "haiku", 9.0})

	got, err := s.ModelStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelStat{
		{Size: "S", Complexity: "low", Model: "sonnet", Tasks: 2, FirstAttemptPass: 0.5, AvgAttempts: 1.5, AvgCostUSD: 3.25},
		{Size: "S", Complexity: "high", Model: "opus", Tasks: 1, FirstAttemptPass: 1, AvgAttempts: 1, AvgCostUSD: 4},
		{Size: "M", Complexity: "low", Model: "sonnet", Tasks: 1, FirstAttemptPass: 1, AvgAttempts: 1, AvgCostUSD: 2},
		{Size: "M", Complexity: "medium", Model: "-", Tasks: 1, FirstAttemptPass: 0, AvgAttempts: 1, AvgCostUSD: 0.25},
	}
	if len(got) != len(want) {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		if g.Size != w.Size || g.Complexity != w.Complexity || g.Model != w.Model || g.Tasks != w.Tasks ||
			!statsModelsNear(g.FirstAttemptPass, w.FirstAttemptPass) || !statsModelsNear(g.AvgAttempts, w.AvgAttempts) ||
			!statsModelsNear(g.AvgCostUSD, w.AvgCostUSD) {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestModelStatsOrdersComplexityLowMediumHighAndModels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	impl := RoleImplementer
	statsModelsTask(t, s, "h", "S", "high", TaskVerified, statsModelsAttempt{1, impl, "a", 0})
	statsModelsTask(t, s, "m2", "S", "medium", TaskVerified, statsModelsAttempt{1, impl, "z", 0})
	statsModelsTask(t, s, "m1", "S", "medium", TaskVerified, statsModelsAttempt{1, impl, "b", 0})
	statsModelsTask(t, s, "l", "S", "low", TaskVerified, statsModelsAttempt{1, impl, "c", 0})
	got, err := s.ModelStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, g := range got {
		order = append(order, g.Complexity+"/"+g.Model)
	}
	want := []string{"low/c", "medium/b", "medium/z", "high/a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestModelStatsEmptyStoreReturnsNoRows(t *testing.T) {
	t.Parallel()
	got, err := newStore(t).ModelStats(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("stats = %+v, %v", got, err)
	}
}

func TestGroupTaskCostsIncludeRetriesOfFinishedTasksInGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	impl, ver := RoleImplementer, "verifier"
	// a: retried once; its cost is every attempt's, the retry on another model included.
	statsModelsTask(t, s, "a", "S", "low", TaskLanded,
		statsModelsAttempt{1, impl, "sonnet", 1.0}, statsModelsAttempt{1, ver, "haiku", 0.5},
		statsModelsAttempt{2, impl, "opus", 3.0})
	statsModelsTask(t, s, "b", "S", "low", TaskFailed, statsModelsAttempt{1, impl, "sonnet", 0.75})
	statsModelsTask(t, s, "c", "S", "low", TaskBlocked, statsModelsAttempt{1, impl, "sonnet", 0.25})
	// Left out: unfinished, other model, other complexity, other size, no implementer attempt.
	statsModelsTask(t, s, "d", "S", "low", TaskRunning, statsModelsAttempt{1, impl, "sonnet", 9})
	statsModelsTask(t, s, "e", "S", "low", TaskVerified, statsModelsAttempt{1, impl, "opus", 9})
	statsModelsTask(t, s, "f", "S", "high", TaskVerified, statsModelsAttempt{1, impl, "sonnet", 9})
	statsModelsTask(t, s, "g", "M", "low", TaskVerified, statsModelsAttempt{1, impl, "sonnet", 9})
	statsModelsTask(t, s, "h", "S", "low", TaskVerified, statsModelsAttempt{1, ver, "sonnet", 9})

	got, err := s.GroupTaskCosts(ctx, "S", "low", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.25, 0.75, 4.5}
	if len(got) != len(want) {
		t.Fatalf("costs = %v, want %v", got, want)
	}
	for i := range want {
		if !statsModelsNear(got[i], want[i]) {
			t.Fatalf("costs = %v, want %v", got, want)
		}
	}
}

func TestGroupTaskCostsEmptyGroupReturnsNone(t *testing.T) {
	t.Parallel()
	got, err := newStore(t).GroupTaskCosts(context.Background(), "S", "low", "sonnet")
	if err != nil || len(got) != 0 {
		t.Fatalf("costs = %v, %v", got, err)
	}
}

// durationStore opens a store whose clock reads *now.
func durationStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := fixed
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "mill.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &now
}

func TestAttemptDurationsPreferAgentMSOverWallTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, now := durationStore(t)
	attempt := func(task string, n int, role, model string, wall, agent time.Duration) {
		t.Helper()
		*now = fixed
		aid, err := s.StartAttempt(ctx, task, n, role)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetAttemptModel(ctx, aid, model, "test"); err != nil {
			t.Fatal(err)
		}
		*now = fixed.Add(wall)
		if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", "", agent); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ id, size, complexity string }{
		{"a", "S", "low"}, {"b", "S", "high"}, {"m", "M", "low"}, {"n", "S", ""},
	} {
		sp := spec(c.id)
		sp.Size, sp.Complexity = c.size, c.complexity
		if err := s.CreateTask(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	attempt("a", 1, RoleImplementer, "sonnet", 10*time.Minute, 0)              // wall time: 600000
	attempt("a", 2, RoleImplementer, "sonnet", 10*time.Minute, 90*time.Second) // agent_ms: 90000
	attempt("a", 1, "verifier", "sonnet", time.Minute, 0)                      // not an implementer
	attempt("a", 3, RoleImplementer, "opus", time.Minute, 0)                   // other model
	attempt("b", 1, RoleImplementer, "sonnet", time.Minute, 0)                 // other complexity
	attempt("m", 1, RoleImplementer, "sonnet", time.Minute, 0)                 // other size
	attempt("n", 1, RoleImplementer, "sonnet", 2*time.Minute, 0)               // no complexity: medium
	if _, err := s.StartAttempt(ctx, "a", 4, RoleImplementer); err != nil {    // still running
		t.Fatal(err)
	}

	got, err := s.AttemptDurations(ctx, "S", "low", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{90000, 600000}; !slices.Equal(got, want) {
		t.Errorf("S/low/sonnet durations = %v, want %v", got, want)
	}
	got, err = s.AttemptDurations(ctx, "S", "medium", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{120000}; !slices.Equal(got, want) {
		t.Errorf("S/medium/sonnet durations = %v, want %v", got, want)
	}
	got, err = s.AttemptDurations(ctx, "S", "low", "haiku")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("S/low/haiku durations = %v, want none", got)
	}
}

func TestGateDurationsMeasureFromGatePhaseToGateResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, now := durationStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.CreateTask(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	gate := func(task string, took time.Duration, data map[string]any) {
		t.Helper()
		*now = fixed
		if err := s.SetTaskStatus(ctx, task, TaskRunning, PhaseGate, ""); err != nil {
			t.Fatal(err)
		}
		*now = fixed.Add(took)
		if _, err := s.AppendEvent(ctx, "task-gate", "", task, data); err != nil {
			t.Fatal(err)
		}
	}
	gate("a", 2*time.Minute, map[string]any{"attempt": 1})
	gate("b", 30*time.Second, map[string]any{"attempt": 1})
	gate("a", time.Second, map[string]any{"attempt": 2, "reused": true}) // reused: no command ran
	// A gate result without a gate phase before it (as publish records) has no start.
	if _, err := s.AppendEvent(ctx, "task-gate", "", "c", map[string]any{"attempt": 1}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GateDurations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{30000, 120000}; !slices.Equal(got, want) {
		t.Errorf("gate durations = %v, want %v", got, want)
	}
}
