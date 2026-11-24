package plan

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/taskspec"
)

func task(id string, deps ...string) taskspec.Spec {
	return taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "do " + id,
		Acceptance: []string{"works"}, Checks: []string{"true"}, Owns: []string{}, DependsOn: deps, Size: "S", Complexity: "medium"}
}

func mk(ts ...taskspec.Spec) Plan { return Plan{Goal: "g", Tasks: ts} }

func TestParseExampleValidatesCleanly(t *testing.T) {
	data, err := os.ReadFile("../../docs/examples/plan-todos.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tasks) == 0 {
		t.Fatal("no tasks")
	}
	if err := p.Validate(Options{}); err != nil {
		t.Fatalf("example should validate: %v", err)
	}
}

func TestParseStrictAndDefaults(t *testing.T) {
	for name, in := range map[string]string{
		"unknown top":  `{"goal":"g","bogus":1,"tasks":[]}`,
		"unknown task": `{"goal":"g","tasks":[{"id":"a","bogus":1}]}`,
		"trailing":     `{"goal":"g","tasks":[]} {}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	p, err := Parse([]byte(`{"goal":"g","tasks":[{"id":"a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a := p.Tasks[0]
	if p.Assumptions == nil || a.Size != "S" || a.Acceptance == nil || a.Checks == nil || a.Owns == nil || a.DependsOn == nil {
		t.Fatalf("defaults not applied: %+v", p)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	bad := task("Bad ID")
	bad.Title = ""
	noChecks := task("nc")
	noChecks.Checks = []string{}
	big := task("big")
	big.Size = "L"
	p := Plan{Tasks: []taskspec.Spec{bad, noChecks, big, task("dup"), task("dup"),
		task("deps", "ghost", "deps", "dup", "dup")}}
	err := p.Validate(Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"goal must not be empty", `task "Bad ID"`, "title must not be empty",
		`task "nc": an implement task needs at least one check`,
		`task "big"`, "split it",
		`task "dup": id is used by 2 tasks`,
		`task "deps": depends_on "ghost" is not a task`,
		`task "deps": depends_on lists the task itself`,
		`task "deps": depends_on lists "dup" twice`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestValidateTaskCounts(t *testing.T) {
	if err := (Plan{Goal: "g"}).Validate(Options{}); err == nil || !strings.Contains(err.Error(), "no tasks") {
		t.Fatalf("got %v", err)
	}
	p := mk(task("a"), task("b"), task("c"))
	if err := p.Validate(Options{MaxTasks: 2}); err == nil || !strings.Contains(err.Error(), "3 tasks") {
		t.Fatalf("got %v", err)
	}
	if err := p.Validate(Options{MaxTasks: 3}); err != nil {
		t.Fatal(err)
	}
	var many []taskspec.Spec
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m"} {
		many = append(many, task(id))
	}
	if err := mk(many...).Validate(Options{}); err == nil {
		t.Fatal("13 tasks should exceed the default of 12")
	}
}

func TestValidateShowsCycle(t *testing.T) {
	p := mk(task("a", "b"), task("b", "a"))
	err := p.Validate(Options{})
	if err == nil || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Fatalf("got %v", err)
	}
	if _, err := p.Order(); err == nil || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Fatalf("Order: got %v", err)
	}
}

func TestOrderDependenciesFirstTiesInPlanOrder(t *testing.T) {
	p := mk(task("a", "c"), task("b"), task("c"), task("d", "a", "b"))
	got, err := p.Order()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c", "a", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// fan-in: top depends on mid1, mid2, leaf; mid1 and mid2 depend on leaf.
func fanIn() Plan {
	return mk(task("leaf"), task("mid1", "leaf"), task("mid2", "leaf"), task("top", "leaf", "mid1", "mid2"), task("solo"))
}

func TestDepth(t *testing.T) {
	p := fanIn()
	for id, want := range map[string]int{"leaf": 0, "solo": 0, "mid1": 1, "top": 2} {
		if got := p.Depth(id); got != want {
			t.Errorf("Depth(%s)=%d want %d", id, got, want)
		}
	}
}

func TestDeepestParentTieGoesToLastListed(t *testing.T) {
	p := fanIn()
	if got := p.DeepestParent("top"); got != "mid2" {
		t.Errorf("got %q want mid2", got)
	}
	if got := p.DeepestParent("mid1"); got != "leaf" {
		t.Errorf("got %q", got)
	}
	if got := p.DeepestParent("leaf"); got != "" {
		t.Errorf("got %q", got)
	}
	q := mk(task("a"), task("b", "a"), task("c"), task("d", "b", "c"))
	if got := q.DeepestParent("d"); got != "b" {
		t.Errorf("got %q want b", got)
	}
}

func TestDependsOnIsTransitive(t *testing.T) {
	p := mk(task("a"), task("b", "a"), task("c", "b"), task("d"))
	cases := []struct {
		a, b string
		want bool
	}{{"c", "b", true}, {"c", "a", true}, {"a", "c", false}, {"d", "a", false}, {"a", "a", false}}
	for _, c := range cases {
		if got := p.DependsOn(c.a, c.b); got != c.want {
			t.Errorf("DependsOn(%s,%s)=%v", c.a, c.b, got)
		}
	}
}

func TestWarningsOnlyForUnrelatedOverlap(t *testing.T) {
	a, b, c := task("a"), task("b"), task("c", "a")
	a.Owns = []string{"internal/store/**"}
	b.Owns = []string{"internal/store/tasks.go"}
	c.Owns = []string{"internal/store/x.go"}
	w := mk(a, b, c).Warnings()
	// a-b overlap (unrelated), a-c overlap but c depends on a, b-c do not overlap.
	if len(w) != 1 {
		t.Fatalf("got %v", w)
	}
	for _, want := range []string{`"a"`, `"b"`, "internal/store/**", "internal/store/tasks.go", "will not run at the same time"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("missing %q in %s", want, w[0])
		}
	}
}

func TestGlobsOverlap(t *testing.T) {
	cases := []struct {
		x, y string
		want bool
	}{
		{"a/x.go", "a/x.go", true},
		{"a/x.go", "a/*.go", true},
		{"internal/store/**", "internal/store/tasks.go", true},
		{"**", "anything/at/all.go", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"a/**/z.go", "a/b/c/z.go", true},
		{"a/**/z.go", "a/z.go", true},
		{"a/x.go", "a/y.go", false},
		{"internal/store/**", "internal/cli/**", false},
		{"docs/a.md", "docs/b/**", false},
		{"a/*.go", "a/b/x.go", false},
	}
	for _, c := range cases {
		if got := GlobsOverlap(c.x, c.y); got != c.want {
			t.Errorf("GlobsOverlap(%q,%q)=%v want %v", c.x, c.y, got, c.want)
		}
		if got := GlobsOverlap(c.y, c.x); got != c.want {
			t.Errorf("GlobsOverlap(%q,%q)=%v want %v", c.y, c.x, got, c.want)
		}
	}
}

func TestMatchOwnsGlobSemantics(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"a/b.go", "a/b.go", true},
		{"a/b.go", "a/c.go", false},
		{"a/*.go", "a/b.go", true},
		{"a/*.go", "a/x/b.go", false},
		{"a/?.go", "a/b.go", true},
		{"a/?.go", "a/bc.go", false},
		{"**/b.go", "b.go", true},
		{"**/b.go", "x/y/b.go", true},
		{"**/b.go", "x/c.go", false},
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"a/**/b.go", "c/x/b.go", false},
		{"a/**", "a/x/y.go", true},
		{"a/**", "a", true},
		{"a/**", "b/x.go", false},
		{"*", "a/b", false},
	}
	for _, c := range cases {
		if got := MatchOwns(c.pat, c.path); got != c.want {
			t.Errorf("MatchOwns(%q,%q)=%v want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestOutsideOwnsKeepsInputOrder(t *testing.T) {
	got := OutsideOwns([]string{"a/**", "x.go"}, []string{"z.go", "a/b.go", "x.go", "m/n.go"})
	if !reflect.DeepEqual(got, []string{"z.go", "m/n.go"}) {
		t.Fatalf("got %v", got)
	}
	if got := OutsideOwns(nil, []string{"z.go"}); got != nil {
		t.Fatalf("empty owns restricted: %v", got)
	}
}

func TestComplexityMissingInStoredPlanParsesAsMedium(t *testing.T) {
	p, err := Parse([]byte(`{"goal":"g","tasks":[{"id":"a","title":"t","kind":"docs","spec":"s"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Tasks[0].Complexity != "medium" {
		t.Fatalf("complexity = %q, want medium", p.Tasks[0].Complexity)
	}
	if err := p.Validate(Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestComplexityInvalidValueNamesTask(t *testing.T) {
	p, err := Parse([]byte(`{"goal":"g","tasks":[{"id":"big-one","title":"t","kind":"docs","spec":"s","complexity":"extreme"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Validate(Options{})
	if err == nil || !strings.Contains(err.Error(), "big-one") || !strings.Contains(err.Error(), "extreme") {
		t.Fatalf("want error naming task and value, got %v", err)
	}
}

func TestComplexityExampleCarriesComplexityOnEveryTask(t *testing.T) {
	data, err := os.ReadFile("../../docs/examples/plan-todos.json")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"complexity":`); n != strings.Count(string(data), `"size":`) {
		t.Fatalf("complexity on %d tasks, size on %d", n, strings.Count(string(data), `"size":`))
	}
}
