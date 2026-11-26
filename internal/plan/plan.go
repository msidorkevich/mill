// Package plan is the plan a goal is broken into: a set of tasks with dependencies, and
// its validation.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/msidorkevich/mill/internal/taskspec"
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

// Warnings lists pairs of tasks with overlapping owns and no dependency path between them,
// followed by the direct dependencies that share only test files.
func (p Plan) Warnings() []string {
	var out []string
	for i, a := range p.Tasks {
		for _, b := range p.Tasks[i+1:] {
			if p.DependsOn(a.ID, b.ID) || p.DependsOn(b.ID, a.ID) {
				continue
			}
			var pats []string
			for _, o := range overlappingOwns(a.Owns, b.Owns) {
				pats = append(pats, o.String())
			}
			if len(pats) > 0 {
				out = append(out, fmt.Sprintf("tasks %q and %q own overlapping paths (%s) and neither depends on the other; they will not run at the same time",
					a.ID, b.ID, strings.Join(pats, ", ")))
			}
		}
	}
	return append(out, p.testOverlapWarnings()...)
}

// testOverlapWarnings warns about every task whose owns overlap with a direct dependency
// consists of test files only.
func (p Plan) testOverlapWarnings() []string {
	idx := p.index()
	var out []string
	for _, b := range p.Tasks {
		for _, id := range b.DependsOn {
			i, ok := idx[id]
			if !ok || id == b.ID {
				continue
			}
			var pats []string
			testOnly := true
			for _, o := range overlappingOwns(p.Tasks[i].Owns, b.Owns) {
				pats = append(pats, o.String())
				testOnly = testOnly && strings.HasSuffix(o.x, "_test.go") && strings.HasSuffix(o.y, "_test.go")
			}
			if len(pats) > 0 && testOnly {
				out = append(out, fmt.Sprintf("%s depends on %s but they share only test files (%s); consider moving the shared test into its own task",
					b.ID, id, strings.Join(pats, ", ")))
			}
		}
	}
	return out
}

// ownsPair is an owns pattern from each of two tasks that overlap.
type ownsPair struct{ x, y string }

func (o ownsPair) String() string {
	if o.x == o.y {
		return fmt.Sprintf("%q", o.x)
	}
	return fmt.Sprintf("%q and %q", o.x, o.y)
}

// overlappingOwns returns every pair of patterns from a and b that GlobsOverlap.
func overlappingOwns(a, b []string) []ownsPair {
	var out []ownsPair
	for _, x := range a {
		for _, y := range b {
			if GlobsOverlap(x, y) {
				out = append(out, ownsPair{x, y})
			}
		}
	}
	return out
}

// WarningsFor is Warnings plus a warning for every task check that is a whole-package
// `go test` without -run while checkCmd already runs go test over everything.
func (p Plan) WarningsFor(checkCmd string) []string {
	out := append(p.Warnings(), p.checkWarnings(checkCmd)...)
	return append(out, p.acceptanceWarnings()...)
}

// acceptanceWarnings warns about every acceptance criterion that asks to change a path owned
// by another task and not by the criterion's own task: its verifier would judge work it cannot
// do. Merely mentioning the path (a fixture, a file to read) is not warned about.
func (p Plan) acceptanceWarnings() []string {
	var out []string
	for _, t := range p.Tasks {
		for _, a := range t.Acceptance {
			for _, path := range criterionPaths(a) {
				if ownsOverlap(t.Owns, path) || !asksChange(a, path) {
					continue
				}
				var others []string
				for _, o := range p.Tasks {
					if o.ID != t.ID && ownsOverlap(o.Owns, path) {
						others = append(others, o.ID)
					}
				}
				if len(others) > 0 {
					out = append(out, fmt.Sprintf("task %s: acceptance %q names %s, owned by %s and not by %s",
						t.ID, a, path, strings.Join(others, ", "), t.ID))
				}
			}
		}
	}
	return out
}

// ownsOverlap reports whether some owns pattern matches or overlaps p.
func ownsOverlap(owns []string, p string) bool {
	for _, o := range owns {
		if MatchOwns(o, p) || GlobsOverlap(o, p) {
			return true
		}
	}
	return false
}

// criterionPaths returns the distinct words of a criterion that look like paths: they contain
// a `/` or end in a file extension.
func criterionPaths(criterion string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(criterion) {
		w = pathWord(w)
		if w == "" || seen[w] || strings.Contains(w, "://") {
			continue
		}
		if strings.Contains(w, "/") || hasFileExt(w) {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// pathWord strips the quoting and punctuation around a word of a criterion.
func pathWord(w string) string {
	const punct = "`'\"()[]{}<>,;:!?"
	w = strings.TrimRight(strings.TrimLeft(w, punct), punct+".")
	return strings.TrimPrefix(w, "./")
}

// changeLookBack is how many words before a path may hold the verb that asks to change it.
const changeLookBack = 4

// changeVerbs are looked for before a path. In a two-word entry the second word may come
// later than directly after the first: "add a section to README.md".
var changeVerbs = [][]string{
	{"update"}, {"change"}, {"modify"}, {"edit"}, {"add", "to"}, {"append", "to"},
	{"remove", "from"}, {"delete"}, {"rename"}, {"write"}, {"create"}, {"document", "in"},
}

// changePredicates are looked for directly after a path, as consecutive words.
var changePredicates = [][]string{
	{"is", "updated"}, {"is", "changed"}, {"is", "edited"}, {"mentions"}, {"documents"},
	{"lists"}, {"describes"}, {"gains"}, {"contains", "a", "new"},
}

// asksChange reports whether some mention of p in the criterion is worded as a change to it:
// a change verb shortly before it, or a change predicate directly after it.
func asksChange(criterion, p string) bool {
	raw := strings.Fields(criterion)
	words := make([]string, len(raw))
	for i, w := range raw {
		words[i] = strings.ToLower(pathWord(w))
	}
	for i, w := range raw {
		if pathWord(w) != p {
			continue
		}
		before := words[max(0, i-changeLookBack):i]
		for _, v := range changeVerbs {
			if containsInOrder(before, v) {
				return true
			}
		}
		// Punctuation after the path ends the clause, so what follows is not about it.
		if strings.ContainsAny(w[len(w)-1:], ".,;:!?)") {
			continue
		}
		for _, pred := range changePredicates {
			if end := i + 1 + len(pred); end <= len(words) && slices.Equal(words[i+1:end], pred) {
				return true
			}
		}
	}
	return false
}

// containsInOrder reports whether every word of want occurs in words, in that order.
func containsInOrder(words, want []string) bool {
	for _, w := range words {
		if len(want) > 0 && w == want[0] {
			want = want[1:]
		}
	}
	return len(want) == 0
}

// hasFileExt reports whether w ends in `.ext` with a non-empty name before it and an
// extension of letters and digits that is not all digits.
func hasFileExt(w string) bool {
	ext := path.Ext(w)
	if len(ext) < 2 || len(ext) == len(w) {
		return false
	}
	letter := false
	for _, r := range ext[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return letter
}

// coversAll reports whether checkCmd runs go test over ./... (`make check` is taken to).
func coversAll(checkCmd string) bool {
	c := strings.Join(strings.Fields(checkCmd), " ")
	return strings.Contains(c, "go test ./...") || c == "make check" || strings.HasPrefix(c, "make check ")
}

// broadGoTest reports whether check is a go test of packages with no -run filter.
func broadGoTest(check string) bool {
	f := strings.Fields(check)
	for i := 0; i+1 < len(f); i++ {
		if f[i] != "go" || f[i+1] != "test" {
			continue
		}
		for _, a := range f[i+2:] {
			if a == "&&" || a == "||" || a == ";" || a == "|" {
				break
			}
			if a == "-run" || strings.HasPrefix(a, "-run=") || strings.HasPrefix(a, "--run") {
				return false
			}
		}
		return true
	}
	return false
}

func (p Plan) checkWarnings(checkCmd string) []string {
	if !coversAll(checkCmd) {
		return nil
	}
	var out []string
	for _, t := range p.Tasks {
		for _, c := range t.Checks {
			if broadGoTest(c) {
				out = append(out, fmt.Sprintf("task %q check %q runs go test without -run, but the gate already runs check_cmd (%s) over everything; narrow it, e.g. `go test -run 'TestName' ./pkg/`",
					t.ID, c, strings.TrimSpace(checkCmd)))
			}
		}
	}
	return out
}

// MatchOwns reports whether path is matched by an owns pattern: slash-separated, `*` and `?`
// match within one segment, `**` matches any number of segments including zero.
func MatchOwns(pattern, p string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(ps, xs []string) bool {
	if len(ps) == 0 {
		return len(xs) == 0
	}
	if ps[0] == "**" {
		if matchSegs(ps[1:], xs) {
			return true
		}
		return len(xs) > 0 && matchSegs(ps, xs[1:])
	}
	if len(xs) == 0 {
		return false
	}
	ok, err := path.Match(ps[0], xs[0])
	return err == nil && ok && matchSegs(ps[1:], xs[1:])
}

// OutsideOwns returns the files matched by no owns pattern, in input order. Empty owns
// means no restriction.
func OutsideOwns(owns, files []string) []string {
	if len(owns) == 0 {
		return nil
	}
	var out []string
	for _, f := range files {
		matched := false
		for _, o := range owns {
			if MatchOwns(o, f) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, f)
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
