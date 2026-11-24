package graph

import (
	"strings"
	"testing"
)

func render(t *testing.T, f func(*strings.Builder) error) string {
	t.Helper()
	var b strings.Builder
	if err := f(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func terminal(t *testing.T, g Graph) string {
	return render(t, func(b *strings.Builder) error { return Terminal(b, g) })
}

func mermaid(t *testing.T, g Graph) string {
	return render(t, func(b *strings.Builder) error { return Mermaid(b, g) })
}

func check(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestGraphDrawsChainInDependencyOrder(t *testing.T) {
	t.Parallel()
	// Input order is not dependency order: rows must put dependencies first.
	g := Graph{
		Nodes: []Node{{"c", "c", "pending"}, {"b", "b", "running"}, {"a", "a", "landed"}},
		Edges: []Edge{{"c", "b"}, {"b", "a"}},
	}
	check(t, terminal(t, g), `* ✓ a  landed
* ▶ b  running  ← a
* ○ c  pending  ← b
3 tasks: 1 landed, 1 running, 1 pending
`)
}

func TestGraphDrawsFanOutAndFanIn(t *testing.T) {
	t.Parallel()
	g := Graph{
		Nodes: []Node{{"a", "a", "landed"}, {"b", "b", "verified"}, {"c", "c", "published"}, {"d", "d", "pending"}},
		Edges: []Edge{{"b", "a"}, {"c", "a"}, {"d", "b"}, {"d", "c"}},
	}
	check(t, terminal(t, g), `*   ✓ a  landed
|\
* | ● b  verified   ← a
| * ↑ c  published  ← a
|/
*   ○ d  pending    ← b, c
4 tasks: 1 landed, 1 published, 1 verified, 1 pending
`)
}

func TestGraphDrawsWideFanOutAndEndingLane(t *testing.T) {
	t.Parallel()
	g := Graph{
		Nodes: []Node{{"a", "a", "landed"}, {"b", "b", "running"}, {"c", "c", "blocked"},
			{"d", "d", "failed"}, {"long", "long", "pending"}},
		Edges: []Edge{{"b", "a"}, {"c", "a"}, {"d", "a"}, {"long", "b"}, {"long", "d"}},
	}
	check(t, terminal(t, g), `*     ✓ a     landed
|\
| |\
* | | ▶ b     running  ← a
| * | ✗ c     blocked  ← a
|  /
| *   ✗ d     failed   ← a
|/
*     ○ long  pending  ← b, d
5 tasks: 1 landed, 1 running, 1 pending, 1 blocked, 1 failed
`)
}

func TestGraphKeepsIndependentTasksApart(t *testing.T) {
	t.Parallel()
	// Two unrelated tasks must not be drawn as consecutive * in one lane (a chain).
	g := Graph{Nodes: []Node{{"x", "x", "landed"}, {"y", "y", "pending"}}}
	check(t, terminal(t, g), `* ✓ x  landed

* ○ y  pending
2 tasks: 1 landed, 1 pending
`)
	// An unrelated task next to a lane still in use gets its own lane.
	g = Graph{
		Nodes: []Node{{"a", "a", "landed"}, {"b", "b", "pending"}, {"c", "c", "pending"}},
		Edges: []Edge{{"c", "a"}},
	}
	check(t, terminal(t, g), `*   ✓ a  landed
| * ○ b  pending
*   ○ c  pending  ← a
3 tasks: 1 landed, 2 pending
`)
}

func TestGraphListsDependenciesInInputOrder(t *testing.T) {
	t.Parallel()
	g := Graph{
		Nodes: []Node{{"m", "m", "pending"}, {"z", "z", "landed"}, {"a", "a", "landed"}},
		Edges: []Edge{{"m", "a"}, {"m", "z"}},
	}
	got := terminal(t, g)
	if !strings.Contains(got, "○ m  pending  ← z, a\n") {
		t.Errorf("dependencies not listed in input order:\n%s", got)
	}
}

func TestGraphRejectsCycleAndUnknownDependency(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	cycle := Graph{Nodes: []Node{{"a", "a", "pending"}, {"b", "b", "pending"}}, Edges: []Edge{{"a", "b"}, {"b", "a"}}}
	if err := Terminal(&b, cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle: err = %v", err)
	}
	unknown := Graph{Nodes: []Node{{"a", "a", "pending"}}, Edges: []Edge{{"a", "zzz"}}}
	if err := Mermaid(&b, unknown); err == nil || !strings.Contains(err.Error(), "zzz") {
		t.Errorf("unknown: err = %v", err)
	}
	if b.Len() != 0 {
		t.Errorf("wrote output on error: %q", b.String())
	}
}

func TestGraphMermaidFanIn(t *testing.T) {
	t.Parallel()
	g := Graph{
		Nodes: []Node{{"merge-all", "merge-all", "pending"}, {"api.v2", "api.v2", "landed"},
			{"end", "end", "failed"}, {"cli", "cli", "landed"}},
		Edges: []Edge{{"merge-all", "api.v2"}, {"merge-all", "end"}, {"merge-all", "cli"}},
	}
	check(t, mermaid(t, g), `flowchart TD
  api_v2["api.v2<br/>landed"]
  n_end["end<br/>failed"]
  cli["cli<br/>landed"]
  merge_all["merge-all<br/>pending"]
  api_v2 --> merge_all
  n_end --> merge_all
  cli --> merge_all
  classDef landed fill:#2da44e,stroke:#1a7f37,color:#ffffff
  classDef pending fill:#d0d7de,stroke:#8c959f,color:#000000
  classDef failed fill:#cf222e,stroke:#a40e26,color:#ffffff
  class api_v2,cli landed
  class merge_all pending
  class n_end failed
`)
}
