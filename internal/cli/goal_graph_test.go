package cli

import (
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/store"
)

const diamondPlan = `{
  "goal": "Diamond",
  "tasks": [
    {"id": "d", "title": "D", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["d.go"], "depends_on": ["b", "c"], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["b.go"], "depends_on": ["a"], "size": "S"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["c.go"], "depends_on": ["a"], "size": "S"},
    {"id": "a", "title": "A", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["a.go"], "depends_on": [], "size": "S"}
  ]
}`

func diamondGoal(t *testing.T) string {
	t.Helper()
	root := planRepo(t)
	if r := run(t, root, nil, "plan", "--file", writePlan(t, diamondPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	setStatus(t, root, "g1/a", store.TaskLanded)
	setStatus(t, root, "g1/b", store.TaskVerified)
	setStatus(t, root, "g1/c", store.TaskRunning)
	return root
}

func TestGoalGraphDrawsLanesInDependencyOrder(t *testing.T) {
	t.Parallel()
	root := diamondGoal(t)
	r := run(t, root, nil, "goal", "graph", "g1")
	if r.code != ExitOK {
		t.Fatalf("graph: %+v", r)
	}
	want := `*   ✓ a  landed
|\
* | ● b  verified  ← a
| * ▶ c  running   ← a
|/
*   ○ d  pending   ← b, c
4 tasks: 1 landed, 1 verified, 1 running, 1 pending
`
	if r.stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", r.stdout, want)
	}
}

func TestGoalGraphMermaidPrintsFlowchart(t *testing.T) {
	t.Parallel()
	root := diamondGoal(t)
	r := run(t, root, nil, "goal", "graph", "--mermaid", "g1")
	if r.code != ExitOK {
		t.Fatalf("graph: %+v", r)
	}
	want := `flowchart TD
  a["a<br/>landed"]
  b["b<br/>verified"]
  c["c<br/>running"]
  d["d<br/>pending"]
  a --> b
  a --> c
  b --> d
  c --> d
  classDef landed fill:#2da44e,stroke:#1a7f37,color:#ffffff
  classDef verified fill:#1b998b,stroke:#137a6f,color:#ffffff
  classDef running fill:#f2cc60,stroke:#bf8700,color:#000000
  classDef pending fill:#d0d7de,stroke:#8c959f,color:#000000
  class a landed
  class b verified
  class c running
  class d pending
`
	if r.stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", r.stdout, want)
	}
}

func TestGoalGraphUnknownGoalExitsOne(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	r := run(t, root, nil, "goal", "graph", "g9")
	if r.code != ExitError || !strings.Contains(r.stderr, "mill goal graph") {
		t.Errorf("unknown goal: %+v", r)
	}
	if u := run(t, root, nil, "goal", "graph"); u.code != ExitUsage {
		t.Errorf("missing goal: %+v", u)
	}
}

func TestGoalGraphDraftGoalHasNoTasks(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, diamondPlan))
	r := run(t, root, nil, "goal", "graph", "g1")
	if r.code != ExitOK || !strings.Contains(r.stdout, "no tasks yet") || !strings.Contains(r.stdout, "mill plan approve g1") {
		t.Errorf("draft: %+v", r)
	}
}
