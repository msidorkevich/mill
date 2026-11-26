package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/skiptest"
)

// appendSkippedTests records a skipped_tests event for task g1/a as the pipeline does.
func appendSkippedTests(t *testing.T, root string, skips ...skiptest.Skip) {
	t.Helper()
	ev := map[string]any{"attempt": 1, "head": "deadbeef", "skips": skips}
	if _, err := openStore(t, root).AppendEvent(context.Background(), "skipped_tests", "", "g1/a", ev); err != nil {
		t.Fatal(err)
	}
}

var twoSkips = []skiptest.Skip{
	{File: "a_test.go", Line: 7, Reason: "flaky on CI"},
	{File: "b_test.go", Line: 3},
}

const twoSkipsSection = "\nSkipped tests added (2):\n  a_test.go:7  flaky on CI\n  b_test.go:3\n"

func TestSkippedTestsShownByTaskShow(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendSkippedTests(t, root, twoSkips...)

	r := run(t, root, nil, "task", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, twoSkipsSection) {
		t.Fatalf("task show: %+v\nwant section:\n%s", r, twoSkipsSection)
	}
}

func TestSkippedTestsShownByReviewShow(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendSkippedTests(t, root, twoSkips...)

	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, twoSkipsSection) {
		t.Fatalf("review show: %+v\nwant section:\n%s", r, twoSkipsSection)
	}
}

func TestSkippedTestsInTaskShowJSON(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendSkippedTests(t, root, twoSkips...)

	r := run(t, root, nil, "task", "show", "g1/a", "--json")
	var v struct {
		SkippedTests []skiptest.Skip `json:"skipped_tests"`
	}
	if r.code != ExitOK {
		t.Fatalf("task show --json: %+v", r)
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if len(v.SkippedTests) != 2 || v.SkippedTests[0] != twoSkips[0] || v.SkippedTests[1] != twoSkips[1] {
		t.Fatalf("skipped_tests = %+v\n%s", v.SkippedTests, r.stdout)
	}
}

func TestSkippedTestsShowsLatestEvent(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendSkippedTests(t, root, skiptest.Skip{File: "old_test.go", Line: 1, Reason: "old"})
	appendSkippedTests(t, root, twoSkips...)

	r := run(t, root, nil, "task", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, twoSkipsSection) || strings.Contains(r.stdout, "old_test.go") {
		t.Fatalf("task show: %+v", r)
	}
}

func TestSkippedTestsAbsentWithoutEvent(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)

	for _, args := range [][]string{{"task", "show", "g1/a"}, {"review", "show", "g1/a"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		if strings.Contains(r.stdout, "Skipped tests") {
			t.Errorf("%v prints a skipped-tests section without an event:\n%s", args, r.stdout)
		}
	}
	r := run(t, root, nil, "task", "show", "g1/a", "--json")
	if r.code != ExitOK || strings.Contains(r.stdout, "skipped_tests") {
		t.Errorf("task show --json has skipped_tests without an event:\n%s", r.stdout)
	}
}
