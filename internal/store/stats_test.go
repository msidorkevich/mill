package store

import (
	"context"
	"math"
	"testing"

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
