package cli

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
)

// planModelsPlan has a mapped task, a task overriding to a priced model and a task overriding
// to an unpriced one.
const planModelsPlan = `{
  "goal": "Models",
  "tasks": [
    {"id": "a", "title": "Mapped", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["a.go"], "size": "S", "complexity": "low", "complexity_reason": "one small function"},
    {"id": "b", "title": "Pinned", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["b.go"], "depends_on": ["a"], "size": "M", "complexity": "high", "complexity_reason": "tricky", "model": "opus"},
    {"id": "c", "title": "Unpriced", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["c.go"], "depends_on": ["b"], "size": "S", "model": "gpt-x"}
  ]
}`

// planModelsGoal creates the draft goal g1 from planModelsPlan in a repo with default config.
func planModelsGoal(t *testing.T) string {
	t.Helper()
	root := planRepo(t)
	if r := run(t, root, nil, "plan", "--file", writePlan(t, planModelsPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	return root
}

func TestPlanShowModelsTextListsComplexityModelAndCost(t *testing.T) {
	t.Parallel()
	root := planModelsGoal(t)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	// Task a, S/low on haiku: low = haiku + sonnet verifier at 100K tokens = 0.018 + 0.36;
	// high = (haiku+sonnet) + (sonnet+sonnet) + (opus+opus) at 200K = 0.756 + 1.44 + 2.88.
	// Task b, M on opus: low = 2 × opus at 300K = 4.32; high = 6 × opus at 600K = 15.552,
	// capped at budget.task.usd = 8.
	for _, want := range []string{
		"kind        implement, size S",
		"complexity  low (one small function)",
		"model       haiku (mapping S/low), est. $0.38–$5.08",
		"kind        implement, size M",
		"complexity  high (tricky)",
		"model       opus (override), est. $4.32–$8.00",
		"complexity  medium (no reason given)",
		"model       gpt-x (override), est. unknown",
		"Estimated cost: $4.70–$13.08, excluding 1 task(s) with unknown cost",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestPlanShowModelsJSONIncludesModelAndCost(t *testing.T) {
	t.Parallel()
	root := planModelsGoal(t)
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
	if a["complexity"] != "low" || a["complexity_reason"] != "one small function" || a["model"] != "haiku" || a["model_why"] != "mapping S/low" {
		t.Errorf("task a: %v", a)
	}
	if lo, _ := a["cost_low_usd"].(float64); math.Abs(lo-0.378) > 1e-9 {
		t.Errorf("task a cost_low_usd = %v, want 0.378", a["cost_low_usd"])
	}
	if hi, _ := a["cost_high_usd"].(float64); math.Abs(hi-5.076) > 1e-9 {
		t.Errorf("task a cost_high_usd = %v, want 5.076", a["cost_high_usd"])
	}
	if b["model"] != "opus" || b["model_why"] != "override" || b["cost_high_usd"] != 8.0 {
		t.Errorf("task b: %v", b)
	}
	for _, k := range []string{"cost_low_usd", "cost_high_usd"} {
		if v, ok := c[k]; !ok || v != nil {
			t.Errorf("task c %s = %v (present %v), want null", k, v, ok)
		}
	}
	if c["model"] != "gpt-x" || c["model_why"] != "override" || c["complexity"] != "medium" {
		t.Errorf("task c: %v", c)
	}
}

func TestPlanShowModelsAllUnpricedPrintsUnknownAndExitsZero(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	body := `{"goal": "U", "tasks": [{"id": "a", "title": "A", "kind": "implement", "spec": "x",
	  "acceptance": ["x"], "checks": ["true"], "owns": ["a.go"], "size": "S", "model": "mystery"}]}`
	if r := run(t, root, nil, "plan", "--file", writePlan(t, body)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	for _, want := range []string{"model       mystery (override), est. unknown", "Estimated cost: unknown"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestPlannerRoleModelIsUsedForPlanning(t *testing.T) {
	t.Parallel()
	root := plannerRepo(t, validPlan)
	cfg := "check_cmd = \"true\"\n[runner]\nkind = \"fake\"\nfake_script = \"script.json\"\n" +
		"[roles]\nplanner = \"fable\"\nverifier_min = \"sonnet\"\n"
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "plan", "add a greeting")
	if r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if !strings.Contains(r.stdout, "Planning (model fable,") {
		t.Errorf("planner did not run on roles.planner:\n%s", r.stdout)
	}
}
