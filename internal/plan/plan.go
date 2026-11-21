// Package plan is the plan a goal is broken into: a set of tasks with dependencies, and
// its validation.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"mill/internal/taskspec"
)

// Plan is a goal broken into tasks.
type Plan struct {
	Goal        string          `json:"goal"`
	Assumptions []string        `json:"assumptions"`
	Tasks       []taskspec.Spec `json:"tasks"`
}

// Options tune validation. MaxTasks 0 means 12.
type Options struct {
	MaxTasks int
}

// Parse decodes a plan strictly and applies per-task defaults. It does not validate.
func Parse(data []byte) (Plan, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("parse plan: %w", err)
	}
	if dec.More() {
		return Plan{}, errors.New("parse plan: trailing data after the plan object")
	}
	if p.Assumptions == nil {
		p.Assumptions = []string{}
	}
	if p.Tasks == nil {
		p.Tasks = []taskspec.Spec{}
	}
	for i := range p.Tasks {
		p.Tasks[i].ApplyDefaults()
	}
	return p, nil
}

// Validate reports every problem at once.
func (p Plan) Validate(o Options) error {
	max := o.MaxTasks
	if max == 0 {
		max = 12
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if strings.TrimSpace(p.Goal) == "" {
		add("goal must not be empty")
	}
	if len(p.Tasks) == 0 {
		add("the plan has no tasks: it needs at least one")
	}
	if len(p.Tasks) > max {
		add("the plan has %d tasks, at most %d are allowed: merge or drop tasks", len(p.Tasks), max)
	}

	count := map[string]int{}
	for _, t := range p.Tasks {
		count[t.ID]++
	}
	reported := map[string]bool{}
	for i, t := range p.Tasks {
		name := fmt.Sprintf("task %q", t.ID)
		if t.ID == "" {
			name = fmt.Sprintf("task #%d", i+1)
		}
		if err := t.ValidateFields(); err != nil {
			for _, e := range unwrapJoin(err) {
				errs = append(errs, fmt.Errorf("%s: %w", name, e))
			}
		}
		if count[t.ID] > 1 && !reported[t.ID] {
			reported[t.ID] = true
			add("%s: id is used by %d tasks, ids must be unique", name, count[t.ID])
		}
		seen := map[string]bool{}
		for _, d := range t.DependsOn {
			switch {
			case d == t.ID:
				add("%s: depends_on lists the task itself", name)
			case count[d] == 0:
				add("%s: depends_on %q is not a task in the plan", name, d)
			case seen[d]:
				add("%s: depends_on lists %q twice", name, d)
			}
			seen[d] = true
		}
		if t.Kind == "implement" && len(t.Checks) == 0 {
			add("%s: an implement task needs at least one check", name)
		}
	}
	if cyc := p.findCycle(); cyc != nil {
		add("dependency cycle: %s", strings.Join(cyc, " -> "))
	}
	return errors.Join(errs...)
}

func unwrapJoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// index maps the first task with each id to its position.
func (p Plan) index() map[string]int {
	m := map[string]int{}
	for i, t := range p.Tasks {
		if _, ok := m[t.ID]; !ok {
			m[t.ID] = i
		}
	}
	return m
}

// findCycle returns a cycle such as [a b a], or nil. Unknown dependencies are ignored.
func (p Plan) findCycle() []string {
	idx := p.index()
	state := make([]int, len(p.Tasks)) // 0 new, 1 on stack, 2 done
	var stack []string
	var visit func(i int) []string
	visit = func(i int) []string {
		state[i] = 1
		stack = append(stack, p.Tasks[i].ID)
		for _, d := range p.Tasks[i].DependsOn {
			j, ok := idx[d]
			if !ok {
				continue
			}
			if state[j] == 1 {
				for k, id := range stack {
					if id == d {
						return append(append([]string{}, stack[k:]...), d)
					}
				}
			}
			if state[j] == 0 {
				if c := visit(j); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[i] = 2
		return nil
	}
	for i := range p.Tasks {
		if state[i] == 0 {
			if c := visit(i); c != nil {
				return c
			}
		}
	}
	return nil
}

// Order returns task ids with every task after its dependencies; ties follow plan order.
func (p Plan) Order() ([]string, error) {
	if cyc := p.findCycle(); cyc != nil {
		return nil, fmt.Errorf("dependency cycle: %s", strings.Join(cyc, " -> "))
	}
	idx := p.index()
	done := make([]bool, len(p.Tasks))
	var order []string
	for len(order) < len(p.Tasks) {
		progressed := false
		for i, t := range p.Tasks {
			if done[i] {
				continue
			}
			ready := true
			for _, d := range t.DependsOn {
				if j, ok := idx[d]; ok && !done[j] {
					ready = false
					break
				}
			}
			if ready {
				done[i] = true
				order = append(order, t.ID)
				progressed = true
				break
			}
		}
		if !progressed {
			return nil, errors.New("plan cannot be ordered: fix its dependencies")
		}
	}
	return order, nil
}

// Depth is the length of the longest dependency chain below the task.
func (p Plan) Depth(id string) int {
	return p.depth(id, map[string]int{}, map[string]bool{})
}

func (p Plan) depth(id string, memo map[string]int, onPath map[string]bool) int {
	if d, ok := memo[id]; ok {
		return d
	}
	i, ok := p.index()[id]
	if !ok || onPath[id] {
		return 0
	}
	onPath[id] = true
	best := 0
	for _, d := range p.Tasks[i].DependsOn {
		if _, ok := p.index()[d]; !ok {
			continue
		}
		if n := p.depth(d, memo, onPath) + 1; n > best {
			best = n
		}
	}
	onPath[id] = false
	memo[id] = best
	return best
}

// DeepestParent is the direct dependency with the greatest Depth, the last listed on a tie.
func (p Plan) DeepestParent(id string) string {
	i, ok := p.index()[id]
	if !ok {
		return ""
	}
	best, bestDepth := "", -1
	for _, d := range p.Tasks[i].DependsOn {
		if n := p.Depth(d); n >= bestDepth {
			best, bestDepth = d, n
		}
	}
	return best
}

// DependsOn reports whether task a depends on task b, directly or transitively.
func (p Plan) DependsOn(a, b string) bool {
	idx := p.index()
	seen := map[string]bool{}
	var walk func(id string) bool
	walk = func(id string) bool {
		i, ok := idx[id]
		if !ok || seen[id] {
			return false
		}
		seen[id] = true
		for _, d := range p.Tasks[i].DependsOn {
			if d == b || walk(d) {
				return true
			}
		}
		return false
	}
	return walk(a)
}

// Warnings lists pairs of tasks with overlapping owns and no dependency path between them.
func (p Plan) Warnings() []string {
	var out []string
	for i, a := range p.Tasks {
		for _, b := range p.Tasks[i+1:] {
			if p.DependsOn(a.ID, b.ID) || p.DependsOn(b.ID, a.ID) {
				continue
			}
			var pats []string
			for _, x := range a.Owns {
				for _, y := range b.Owns {
					if GlobsOverlap(x, y) {
						if x == y {
							pats = append(pats, fmt.Sprintf("%q", x))
						} else {
							pats = append(pats, fmt.Sprintf("%q and %q", x, y))
						}
					}
				}
			}
			if len(pats) > 0 {
				out = append(out, fmt.Sprintf("tasks %q and %q own overlapping paths (%s) and neither depends on the other; they will not run at the same time",
					a.ID, b.ID, strings.Join(pats, ", ")))
			}
		}
	}
	return out
}

// GlobsOverlap reports whether some path could match both patterns. It may over-report
// when both patterns have a wildcard in the same segment, but never under-reports.
func GlobsOverlap(x, y string) bool {
	return overlap(strings.Split(x, "/"), strings.Split(y, "/"))
}

func overlap(xs, ys []string) bool {
	if len(xs) == 0 && len(ys) == 0 {
		return true
	}
	if len(xs) > 0 && xs[0] == "**" {
		if overlap(xs[1:], ys) || (len(ys) > 0 && overlap(xs, ys[1:])) {
			return true
		}
		return false
	}
	if len(ys) > 0 && ys[0] == "**" {
		return overlap(ys, xs)
	}
	if len(xs) == 0 || len(ys) == 0 {
		return false
	}
	return segOverlap(xs[0], ys[0]) && overlap(xs[1:], ys[1:])
}

func isWild(s string) bool { return strings.ContainsAny(s, "*?") }

func segOverlap(a, b string) bool {
	switch {
	case isWild(a) && isWild(b):
		return true
	case isWild(a):
		ok, err := path.Match(a, b)
		return ok || err != nil
	case isWild(b):
		ok, err := path.Match(b, a)
		return ok || err != nil
	}
	return a == b
}
