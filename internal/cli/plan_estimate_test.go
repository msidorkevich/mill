package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// planEstimateSeed records finished S/low tasks whose first implementer attempt ran on haiku,
// one per cost. The first task's cost is split over an attempt and its retry.
func planEstimateSeed(t *testing.T, root string, costs ...float64) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, root)
	for i, cost := range costs {
		id := fmt.Sprintf("past%d", i)
		if err := s.CreateTask(ctx, taskspec.Spec{ID: id, Title: id, Kind: "implement", Size: "S", Complexity: "low"}); err != nil {
			t.Fatal(err)
		}
		parts := []float64{cost}
		if i == 0 {
			parts = []float64{cost / 2, cost / 2}
		}
		for n, part := range parts {
			aid, err := s.StartAttempt(ctx, id, n+1, store.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetAttemptModel(ctx, aid, "haiku", "test"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{CostUSD: part}, false, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetTaskStatus(ctx, id, store.TaskLanded, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanEstimateUsesPastTaskCostsWithThreeOrMore(t *testing.T) {
	t.Parallel()
	root := planModelsGoal(t)
	// Sorted costs 0.2, 0.4, 0.6, 0.8, 2.0: 25th percentile 0.4, 90th 0.8 + 0.6 × 1.2 = 1.52,
	// average 0.8. Task a (S/low on haiku) uses them; b keeps its budget-based range.
	planEstimateSeed(t, root, 2.0, 0.2, 0.4, 0.6, 0.8)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	for _, want := range []string{
		"model       haiku (mapping S/low), est. $0.40–$1.52 (avg $0.80, from 5 past tasks)",
		"model       opus (override), est. $4.32–$8.00 (from budget limits)",
		"model       gpt-x (override), est. unknown",
		"Estimated cost: $4.72–$9.52 (1 from past tasks, 1 from budget limits), excluding 1 task(s) with unknown cost",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestPlanEstimateFallsBackToBudgetWithFewerThanThree(t *testing.T) {
	t.Parallel()
	root := planModelsGoal(t)
	planEstimateSeed(t, root, 0.2, 0.4)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	for _, want := range []string{
		"model       haiku (mapping S/low), est. $0.38–$5.08 (from budget limits)",
		"Estimated cost: $4.70–$13.08 (0 from past tasks, 2 from budget limits), excluding 1 task(s) with unknown cost",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "past tasks)") {
		t.Errorf("output uses past tasks:\n%s", r.stdout)
	}
}

func TestPlanEstimateJSONIncludesCostBasisAndTasks(t *testing.T) {
	t.Parallel()
	root := planModelsGoal(t)
	planEstimateSeed(t, root, 2.0, 0.2, 0.4, 0.6, 0.8)
	r := run(t, root, nil, "plan", "show", "g1", "--json")
	if r.code != ExitOK {
		t.Fatalf("plan show --json: %+v", r)
	}
	var doc struct {
		Order []map[string]any `json:"order"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, r.stdout)
	}
	if len(doc.Order) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(doc.Order))
	}
	a, b, c := doc.Order[0], doc.Order[1], doc.Order[2]
	if a["cost_basis"] != "stats" || a["cost_tasks"] != 5.0 {
		t.Errorf("task a: %v", a)
	}
	if lo, _ := a["cost_low_usd"].(float64); math.Abs(lo-0.4) > 1e-9 {
		t.Errorf("task a cost_low_usd = %v, want 0.4", a["cost_low_usd"])
	}
	if hi, _ := a["cost_high_usd"].(float64); math.Abs(hi-1.52) > 1e-9 {
		t.Errorf("task a cost_high_usd = %v, want 1.52", a["cost_high_usd"])
	}
	for name, x := range map[string]map[string]any{"b": b, "c": c} {
		if x["cost_basis"] != "budget" || x["cost_tasks"] != 0.0 {
			t.Errorf("task %s: %v", name, x)
		}
	}
}
