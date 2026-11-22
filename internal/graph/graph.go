// Package graph renders a task graph: as a lane drawing for the terminal (like
// `git log --graph`, dependencies above their dependents) and as a Mermaid flowchart.
// It works on plain nodes and edges so any view of tasks can reuse it.
package graph

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Node is one task in the graph.
type Node struct {
	ID     string // unique within the graph; edges refer to it
	Label  string // what is shown for the node (for tasks: the local id)
	Status string // task status: landed, published, verified, running, pending, blocked, failed
}

// Edge says that task From depends on task To.
type Edge struct {
	From string
	To   string
}

// Graph is a set of nodes and the dependency edges between them.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// statusOrder is the order statuses are counted and styled in.
var statusOrder = []string{"landed", "published", "verified", "running", "pending", "blocked", "failed"}

var marks = map[string]string{
	"landed":    "✓",
	"published": "↑",
	"verified":  "●",
	"running":   "▶",
	"pending":   "○",
	"blocked":   "✗",
	"failed":    "✗",
}

// Mark returns the status mark for status ("?" for an unknown status).
func Mark(status string) string {
	if m, ok := marks[status]; ok {
		return m
	}
	return "?"
}

// order returns the node indexes with every dependency before its dependents, keeping the
// input order where the dependencies allow it, and each node's dependents in that order.
func (g Graph) order() ([]int, map[string][]string, error) {
	index := map[string]int{}
	for i, n := range g.Nodes {
		if _, dup := index[n.ID]; dup {
			return nil, nil, fmt.Errorf("graph: node %q appears twice", n.ID)
		}
		index[n.ID] = i
	}
	deps := make([]map[string]bool, len(g.Nodes))
	for i := range deps {
		deps[i] = map[string]bool{}
	}
	for _, e := range g.Edges {
		from, ok := index[e.From]
		if !ok {
			return nil, nil, fmt.Errorf("graph: edge from unknown node %q", e.From)
		}
		if _, ok := index[e.To]; !ok {
			return nil, nil, fmt.Errorf("graph: %q depends on unknown node %q", e.From, e.To)
		}
		deps[from][e.To] = true
	}
	done := map[string]bool{}
	var out []int
	for len(out) < len(g.Nodes) {
		next := -1
		for i, n := range g.Nodes {
			if done[n.ID] {
				continue
			}
			ready := true
			for d := range deps[i] {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				next = i
				break
			}
		}
		if next < 0 {
			return nil, nil, fmt.Errorf("graph: the dependencies form a cycle")
		}
		done[g.Nodes[next].ID] = true
		out = append(out, next)
	}
	dependents := map[string][]string{}
	for _, i := range out {
		for _, j := range out {
			if deps[j][g.Nodes[i].ID] {
				dependents[g.Nodes[i].ID] = append(dependents[g.Nodes[i].ID], g.Nodes[j].ID)
			}
		}
	}
	return out, dependents, nil
}

// line is one line of the drawing: lane art and, for a node row, the node it shows.
type line struct {
	art  string
	node int // -1 for a connecting line
}

// lane is an edge on its way down to target, currently drawn in column pos.
type lane struct {
	target string
	pos    int
}

// moves draws the lines that bring every lane from its pos to the column dest[i], one
// column per line ('/' left, '\' right, '|' staying). Lanes move in one direction only.
func moves(lanes []lane, dest []int) []line {
	var out []line
	for {
		moving := false
		for i, l := range lanes {
			if l.pos != dest[i] {
				moving = true
				break
			}
		}
		if !moving {
			return out
		}
		var b []byte
		put := func(at int, c byte) {
			for len(b) <= at {
				b = append(b, ' ')
			}
			b[at] = c
		}
		for i := range lanes {
			switch p := lanes[i].pos; {
			case p > dest[i]:
				put(2*p-1, '/')
				lanes[i].pos--
			case p < dest[i]:
				put(2*p+1, '\\')
				lanes[i].pos++
			default:
				put(2*p, '|')
			}
		}
		out = append(out, line{art: string(b), node: -1})
	}
}

// Terminal writes the lane drawing: one row per node in dependency order,
// "<lane art> <mark> <label>  <status>", followed by "  ← dep1, dep2" (the labels of the
// node's direct dependencies, in input order) when it has any, then a line with the counts
// by status. Unrelated nodes never share a lane on consecutive rows.
func Terminal(w io.Writer, g Graph) error {
	order, dependents, err := g.order()
	if err != nil {
		return err
	}
	var lines []line
	var lanes []string // the target of each column
	for _, i := range order {
		id := g.Nodes[i].ID
		col := -1
		for c, t := range lanes {
			if t == id {
				col = c
				break
			}
		}
		if col < 0 {
			// A task nothing leads to starts a new lane. Never put it right under the
			// previous row's task (whose lane just ended): adjacent * read as a chain.
			col = len(lanes)
			if n := len(lines); n > 0 && lines[n-1].node >= 0 &&
				2*col < len(lines[n-1].art) && lines[n-1].art[2*col] == '*' {
				lines = append(lines, line{art: strings.Repeat("| ", len(lanes)), node: -1})
			}
			lanes = append(lanes, id)
		}
		row := make([]byte, 2*len(lanes)-1)
		for c := range row {
			row[c] = ' '
		}
		for c := range lanes {
			row[2*c] = '|'
		}
		row[2*col] = '*'
		lines = append(lines, line{art: string(row), node: i})

		// Fan-out: the node's column becomes one lane per dependent (none ends it).
		var ls []lane
		var next []string
		for c, t := range lanes {
			if c != col {
				ls = append(ls, lane{t, c})
				next = append(next, t)
				continue
			}
			for _, d := range dependents[id] {
				ls = append(ls, lane{d, c})
				next = append(next, d)
			}
		}
		dest := make([]int, len(ls))
		for k := range dest {
			dest[k] = k
		}
		lines = append(lines, moves(ls, dest)...)

		// Fan-in: lanes heading to the same node join its leftmost lane.
		first := map[string]int{}
		lanes = nil
		ls = ls[:0]
		dest = dest[:0]
		for k, t := range next {
			if _, ok := first[t]; !ok {
				first[t] = len(lanes)
				lanes = append(lanes, t)
			}
			ls = append(ls, lane{t, k})
			dest = append(dest, first[t])
		}
		lines = append(lines, moves(ls, dest)...)
	}

	index := map[string]int{}
	for i, n := range g.Nodes {
		index[n.ID] = i
	}
	depOf := make([][]bool, len(g.Nodes))
	for i := range depOf {
		depOf[i] = make([]bool, len(g.Nodes))
	}
	for _, e := range g.Edges {
		depOf[index[e.From]][index[e.To]] = true
	}
	width, labelWidth, statusWidth := 0, 0, 0
	for _, l := range lines {
		if l.node >= 0 {
			width = max(width, len(l.art))
			labelWidth = max(labelWidth, len([]rune(g.Nodes[l.node].Label)))
			statusWidth = max(statusWidth, len([]rune(g.Nodes[l.node].Status)))
		}
	}
	var b strings.Builder
	for _, l := range lines {
		if l.node < 0 {
			b.WriteString(strings.TrimRight(l.art, " ") + "\n")
			continue
		}
		n := g.Nodes[l.node]
		pad := strings.Repeat(" ", labelWidth-len([]rune(n.Label)))
		var deps []string
		for j, d := range depOf[l.node] {
			if d {
				deps = append(deps, g.Nodes[j].Label)
			}
		}
		fmt.Fprintf(&b, "%-*s %s %s%s  %s", width, l.art, Mark(n.Status), n.Label, pad, n.Status)
		if len(deps) > 0 {
			pad := strings.Repeat(" ", statusWidth-len([]rune(n.Status)))
			fmt.Fprintf(&b, "%s  ← %s", pad, strings.Join(deps, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString(Counts(g) + "\n")
	_, err = io.WriteString(w, b.String())
	return err
}

// Counts returns the line with the number of nodes by status, e.g.
// "4 tasks: 1 landed, 1 verified, 2 pending".
func Counts(g Graph) string {
	count := map[string]int{}
	for _, n := range g.Nodes {
		count[n.Status]++
	}
	var parts []string
	for _, s := range statuses(count) {
		parts = append(parts, fmt.Sprintf("%d %s", count[s], s))
	}
	noun := "tasks"
	if len(g.Nodes) == 1 {
		noun = "task"
	}
	if len(parts) == 0 {
		return "0 tasks"
	}
	return fmt.Sprintf("%d %s: %s", len(g.Nodes), noun, strings.Join(parts, ", "))
}

// statuses returns the statuses present in count: known ones in statusOrder, then the rest sorted.
func statuses(count map[string]int) []string {
	var out []string
	known := map[string]bool{}
	for _, s := range statusOrder {
		known[s] = true
		if count[s] > 0 {
			out = append(out, s)
		}
	}
	var rest []string
	for s := range count {
		if !known[s] {
			rest = append(rest, s)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

var classDefs = map[string]string{
	"landed":    "fill:#2da44e,stroke:#1a7f37,color:#ffffff",
	"published": "fill:#0969da,stroke:#0550ae,color:#ffffff",
	"verified":  "fill:#1b998b,stroke:#137a6f,color:#ffffff",
	"running":   "fill:#f2cc60,stroke:#bf8700,color:#000000",
	"pending":   "fill:#d0d7de,stroke:#8c959f,color:#000000",
	"blocked":   "fill:#cf222e,stroke:#a40e26,color:#ffffff",
	"failed":    "fill:#cf222e,stroke:#a40e26,color:#ffffff",
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// mermaidIDs maps each node id to a unique id Mermaid accepts.
func mermaidIDs(nodes []Node) map[string]string {
	out := map[string]string{}
	used := map[string]bool{}
	for _, n := range nodes {
		base := unsafe.ReplaceAllString(n.ID, "_")
		if base == "" || strings.EqualFold(base, "end") {
			base = "n_" + base
		}
		id := base
		for k := 2; used[id]; k++ {
			id = fmt.Sprintf("%s_%d", base, k)
		}
		used[id] = true
		out[n.ID] = id
	}
	return out
}

// Mermaid writes a `flowchart TD` with one node per task labelled with its label and status,
// an edge from each dependency to its dependent, and a classDef per status applied to the nodes.
func Mermaid(w io.Writer, g Graph) error {
	order, dependents, err := g.order()
	if err != nil {
		return err
	}
	ids := mermaidIDs(g.Nodes)
	esc := strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;")
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	for _, i := range order {
		n := g.Nodes[i]
		fmt.Fprintf(&b, "  %s[\"%s<br/>%s\"]\n", ids[n.ID], esc.Replace(n.Label), esc.Replace(n.Status))
	}
	for _, i := range order {
		id := g.Nodes[i].ID
		for _, d := range dependents[id] {
			fmt.Fprintf(&b, "  %s --> %s\n", ids[id], ids[d])
		}
	}
	byStatus := map[string][]string{}
	count := map[string]int{}
	for _, i := range order {
		n := g.Nodes[i]
		byStatus[n.Status] = append(byStatus[n.Status], ids[n.ID])
		count[n.Status]++
	}
	for _, s := range statuses(count) {
		if def, ok := classDefs[s]; ok {
			fmt.Fprintf(&b, "  classDef %s %s\n", s, def)
		}
	}
	for _, s := range statuses(count) {
		if _, ok := classDefs[s]; ok {
			fmt.Fprintf(&b, "  class %s %s\n", strings.Join(byStatus[s], ","), s)
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}
