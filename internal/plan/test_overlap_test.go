package plan

import (
	"reflect"
	"strings"
	"testing"
)

func TestWarnsWhenDependentTasksShareOnlyTestFiles(t *testing.T) {
	t.Parallel()
	a, b := task("a"), task("b", "a")
	a.Owns = []string{"internal/a/a.go", "internal/x/x_test.go", "internal/y/*_test.go"}
	b.Owns = []string{"internal/b/b.go", "internal/x/x_test.go", "internal/y/y_test.go"}
	p := mk(a, b)
	want := []string{`b depends on a but they share only test files ("internal/x/x_test.go", "internal/y/*_test.go" and "internal/y/y_test.go"); consider moving the shared test into its own task`}
	if w := p.Warnings(); !reflect.DeepEqual(w, want) {
		t.Errorf("got %q, want %q", w, want)
	}
	if err := p.Validate(Options{}); err != nil {
		t.Errorf("warnings must not fail validation: %v", err)
	}
}

func TestNoTestOverlapWarningWhenNonTestFilesShared(t *testing.T) {
	t.Parallel()
	for name, shared := range map[string]string{
		"source file": "internal/x/x.go",
		"broad glob":  "internal/x/**",
	} {
		a, b := task("a"), task("b", "a")
		a.Owns = []string{"internal/x/x_test.go", shared}
		b.Owns = []string{"internal/x/x_test.go", shared}
		if w := mk(a, b).Warnings(); len(w) != 0 {
			t.Errorf("%s: got %v", name, w)
		}
	}
}

func TestNoTestOverlapWarningWithoutDependency(t *testing.T) {
	t.Parallel()
	a, b := task("a"), task("b")
	a.Owns = []string{"internal/x/x_test.go"}
	b.Owns = []string{"internal/x/x_test.go"}
	w := mk(a, b).Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "neither depends on the other") {
		t.Errorf("want only the unrelated-overlap warning, got %v", w)
	}
}
