package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// planChainPlan has the chain a → b → c (most tasks) and a → d, where d runs on a slower
// model than the others.
const planChainPlan = `{
  "goal": "Chain",
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["a.go"], "size": "S", "complexity": "low", "model": "haiku"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["b.go"], "depends_on": ["a"], "size": "S", "complexity": "low", "model": "haiku"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["c.go"], "depends_on": ["b"], "size": "S", "complexity": "low", "model": "haiku"},
    {"id": "d", "title": "D", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"],
     "owns": ["d.go"], "depends_on": ["a"], "size": "M", "complexity": "high", "model": "opus"}
  ]
}`

func planChainGoal(t *testing.T) string {
	t.Helper()
	root := planRepo(t)
	if r := run(t, root, nil, "plan", "--file", writePlan(t, planChainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	return root
}

// planChainHistory opens the project's store with a clock the seed functions move.
type planChainHistory struct {
	t   *testing.T
	s   *store.Store
	now time.Time
	n   int
}

func newPlanChainHistory(t *testing.T, root string) *planChainHistory {
	t.Helper()
	h := &planChainHistory{t: t, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, err := store.Open(context.Background(), project.PathsAt(root).DB, store.Options{Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h.s = s
	return h
}

// task creates a past task of size and complexity.
func (h *planChainHistory) task(size, complexity string) string {
	h.t.Helper()
	h.n++
	id := fmt.Sprintf("past%d", h.n)
	if err := h.s.CreateTask(context.Background(), taskspec.Spec{ID: id, Title: id, Kind: "implement", Size: size, Complexity: complexity}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// attempt records a finished implementer attempt on model taking wall time, with agent as its
// agent-reported duration (0: unknown).
func (h *planChainHistory) attempt(size, complexity, model string, wall, agent time.Duration) {
	h.t.Helper()
	ctx := context.Background()
	id := h.task(size, complexity)
	aid, err := h.s.StartAttempt(ctx, id, 1, store.RoleImplementer)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.s.SetAttemptModel(ctx, aid, model, "test"); err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(wall)
	if err := h.s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", "", agent); err != nil {
		h.t.Fatal(err)
	}
}

// gate records a gate that ran for took.
func (h *planChainHistory) gate(took time.Duration) {
	h.t.Helper()
	ctx := context.Background()
	id := h.task("S", "low")
	if err := h.s.SetTaskStatus(ctx, id, store.TaskRunning, store.PhaseGate, ""); err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(took)
	if _, err := h.s.AppendEvent(ctx, "task-gate", "", id, map[string]any{"attempt": 1}); err != nil {
		h.t.Fatal(err)
	}
}

// seedPlanChain records history where haiku S/low attempts take a median 40s (agent-reported;
// their wall-time median would be 50s), opus M/high attempts 5m and gates a median 60s. Then
// a, b and c are estimated at 100s each and d at 360s: a → d (460s) outlasts a → b → c (300s).
func seedPlanChain(t *testing.T, root string) {
	h := newPlanChainHistory(t, root)
	h.attempt("S", "low", "haiku", 10*time.Minute, 30*time.Second)
	h.attempt("S", "low", "haiku", 40*time.Second, 0)
	h.attempt("S", "low", "haiku", 50*time.Second, 0)
	h.attempt("M", "high", "opus", 5*time.Minute, 0)
	h.gate(60 * time.Second)
	h.gate(20 * time.Second)
	h.gate(100 * time.Second)
}

type planChainJSON struct {
	LongestChain struct {
		IDs        []string `json:"ids"`
		EstimateMS *int64   `json:"estimate_ms"`
	} `json:"longest_chain"`
}

func TestPlanShowLongestChainByEstimatedDuration(t *testing.T) {
	t.Parallel()
	root := planChainGoal(t)
	seedPlanChain(t, root)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	// (40s + 60s) + (5m + 60s) = 7m40s.
	if want := "Longest chain: a → d (2 tasks), est. duration 7m40s\n"; !strings.Contains(r.stdout, want) {
		t.Errorf("output missing %q:\n%s", want, r.stdout)
	}
}

func TestPlanShowLongestChainPrefersAgentDurationOverWallTime(t *testing.T) {
	t.Parallel()
	root := planChainGoal(t)
	h := newPlanChainHistory(t, root)
	// Agent-reported 30s each, though each attempt's wall time is 10m.
	h.attempt("S", "low", "haiku", 10*time.Minute, 30*time.Second)
	h.attempt("S", "low", "haiku", 10*time.Minute, 30*time.Second)
	h.attempt("M", "high", "opus", 20*time.Second, 0)
	h.gate(10 * time.Second)
	r := run(t, root, nil, "plan", "show", "g1", "--json")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	var got planChainJSON
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.stdout)
	}
	// Each haiku task 30s + 10s = 40s, d 30s: a → b → c = 120s beats a → d = 70s.
	c := got.LongestChain
	if !slices.Equal(c.IDs, []string{"a", "b", "c"}) || c.EstimateMS == nil || *c.EstimateMS != 120000 {
		t.Errorf("longest_chain = %v, %v; want [a b c], 120000", c.IDs, c.EstimateMS)
	}
}

func TestPlanShowLongestChainJSONHasIDsAndEstimate(t *testing.T) {
	t.Parallel()
	root := planChainGoal(t)
	seedPlanChain(t, root)
	r := run(t, root, nil, "plan", "show", "g1", "--json")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	var got planChainJSON
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.stdout)
	}
	c := got.LongestChain
	if !slices.Equal(c.IDs, []string{"a", "d"}) || c.EstimateMS == nil || *c.EstimateMS != 460000 {
		t.Errorf("longest_chain = %v, %v; want [a d], 460000", c.IDs, c.EstimateMS)
	}
}

func TestPlanShowLongestChainWithoutHistoryIsUnknown(t *testing.T) {
	t.Parallel()
	root := planChainGoal(t)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	if want := "Longest chain: a → b → c (3 tasks), est. duration unknown"; !strings.Contains(r.stdout, want) {
		t.Errorf("output missing %q:\n%s", want, r.stdout)
	}

	r = run(t, root, nil, "plan", "show", "g1", "--json")
	if r.code != ExitOK {
		t.Fatalf("plan show --json: %+v", r)
	}
	if !strings.Contains(r.stdout, `"estimate_ms": null`) {
		t.Errorf("estimate_ms is not null:\n%s", r.stdout)
	}
	var got planChainJSON
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.stdout)
	}
	if c := got.LongestChain; !slices.Equal(c.IDs, []string{"a", "b", "c"}) || c.EstimateMS != nil {
		t.Errorf("longest_chain = %v, %v; want [a b c], null", c.IDs, c.EstimateMS)
	}
}

func TestPlanShowLongestChainUnknownWhenATaskHasNoHistory(t *testing.T) {
	t.Parallel()
	root := planChainGoal(t)
	h := newPlanChainHistory(t, root)
	// Gates and haiku attempts, but no opus M/high attempt for d.
	h.attempt("S", "low", "haiku", time.Minute, 0)
	h.gate(time.Minute)
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("plan show: %+v", r)
	}
	if want := "Longest chain: a → b → c (3 tasks), est. duration unknown"; !strings.Contains(r.stdout, want) {
		t.Errorf("output missing %q:\n%s", want, r.stdout)
	}
}
