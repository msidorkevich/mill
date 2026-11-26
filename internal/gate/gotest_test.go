package gate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	plainFailure = "=== RUN   TestA\n--- FAIL: TestA (0.00s)\n    a_test.go:5: want 1, got 2\n" +
		"FAIL\nFAIL\tgithub.com/x/y\t0.01s\nFAIL\n"
	subtestFailure = "--- FAIL: TestA (0.00s)\n    --- FAIL: TestA/case_1 (0.00s)\n        a_test.go:9: boom\n" +
		"    --- PASS: TestA/case_2 (0.00s)\nFAIL\nFAIL\tgithub.com/x/y\t0.01s\n"
	buildFailure = "# github.com/x/y\n./a.go:3:2: undefined: foo\nFAIL\tgithub.com/x/y [build failed]\nFAIL\n"
	panicFailure = "panic: kaboom\n\ngoroutine 1 [running]:\n" +
		"main.init()\n\t/tmp/a.go:3 +0x1\nexit status 2\nFAIL\tgithub.com/x/p\t0.01s\n"
	mixedPackages = "ok  \tgithub.com/x/a\t0.01s\n--- FAIL: TestB (0.00s)\nFAIL\nFAIL\tgithub.com/x/b\t0.01s\n" +
		"ok  \tgithub.com/x/c\t(cached)\npanic: kaboom\nFAIL\tgithub.com/x/d\t0.01s\n" +
		"--- FAIL: TestB (0.00s)\nFAIL\tgithub.com/x/e\t0.01s\nFAIL\n"
	consecutiveBuildFailures = "# x/a\na.go:3: undefined: foo\n# x/b [x/b.test]\nb_test.go:4: undefined: bar\n" +
		"FAIL\tx/a [build failed]\nFAIL\tx/b [build failed]\nFAIL\n"
)

func TestGoTestFailuresReportsTestsAndPackages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output string
		want         []string
	}{
		{"plain test failure", plainFailure, []string{"TestA"}},
		{"nested subtest failure", subtestFailure, []string{"TestA", "TestA/case_1"}},
		{"build failure", buildFailure, []string{"pkg:github.com/x/y"}},
		{"consecutive build failures", consecutiveBuildFailures, []string{"pkg:x/a", "pkg:x/b"}},
		{"panic without a failing test", panicFailure, []string{"pkg:github.com/x/p"}},
		{"mixed packages, sorted and de-duplicated", mixedPackages, []string{"TestB", "pkg:github.com/x/d"}},
		{"non-go output", "npm ERR! test failed\nFAIL src/a.test.js\nok then\n", nil},
		{"passing run", "=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\nok  \tgithub.com/x/y\t0.01s\n", nil},
		{"bare FAIL line only", "FAIL\n", nil},
		{"empty", "", nil},
	} {
		got, ok := GoTestFailures(tc.output)
		if !slices.Equal(got, tc.want) || ok != (tc.want != nil) {
			t.Errorf("%s: failures = %q, ok = %v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestGoTestFailuresFromLogReadsFileAndReportsReadErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "gate-1.log")
	if err := os.WriteFile(path, []byte(plainFailure), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GoTestFailuresFromLog(path)
	if err != nil || !ok || !slices.Equal(got, []string{"TestA"}) {
		t.Fatalf("from log = %q, %v, %v", got, ok, err)
	}
	if _, ok, err := GoTestFailuresFromLog(filepath.Join(dir, "missing.log")); ok || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing log: ok = %v, err = %v", ok, err)
	}
}

func TestGoTestExcerptShowsOutputOfNamedFailures(t *testing.T) {
	t.Parallel()
	var trace strings.Builder
	for i := range 2 * goPkgContext {
		fmt.Fprintf(&trace, "frame %d\n", i)
	}
	for _, tc := range []struct {
		name, output string
		names        []string
		want         string
	}{
		{"test with its indented lines", plainFailure, []string{"TestA"},
			"--- FAIL: TestA (0.00s)\n    a_test.go:5: want 1, got 2"},
		{"subtest", subtestFailure, []string{"TestA/case_1"},
			"    --- FAIL: TestA/case_1 (0.00s)\n        a_test.go:9: boom"},
		{"parent stops at its subtest", subtestFailure, []string{"TestA"},
			"--- FAIL: TestA (0.00s)"},
		{"build failure", buildFailure, []string{"pkg:github.com/x/y"},
			"# github.com/x/y\n./a.go:3:2: undefined: foo\nFAIL\tgithub.com/x/y [build failed]"},
		{"package stops at the previous package", mixedPackages, []string{"pkg:github.com/x/d"},
			"panic: kaboom\nFAIL\tgithub.com/x/d\t0.01s"},
		{"long panic keeps its message", "panic: kaboom\n" + trace.String() + "FAIL\tp\t0.01s\n", []string{"pkg:p"},
			"panic: kaboom\n" + trace.String() + "FAIL\tp\t0.01s"},
		{"build failure after another package's result", "# x/b\nb.go:3: undefined: foo\nok  \tx/a\t0.1s\nFAIL\tx/b [build failed]\n",
			[]string{"pkg:x/b"}, "# x/b\nb.go:3: undefined: foo\nFAIL\tx/b [build failed]"},
		{"consecutive build failures", consecutiveBuildFailures, []string{"pkg:x/a", "pkg:x/b"},
			"# x/a\na.go:3: undefined: foo\nFAIL\tx/a [build failed]\n" +
				"# x/b [x/b.test]\nb_test.go:4: undefined: bar\nFAIL\tx/b [build failed]"},
		{"build failure of a dependency", "# x/dep\ndep.go:1: syntax error\nFAIL\tx/a [build failed]\nFAIL\tx/b [build failed]\n",
			[]string{"pkg:x/b"}, "# x/dep\ndep.go:1: syntax error\nFAIL\tx/b [build failed]"},
		{"unknown name", plainFailure, []string{"TestZ", "pkg:nope"}, ""},
	} {
		if got := GoTestExcerpt(tc.output, tc.names, 100); got != tc.want {
			t.Errorf("%s: excerpt = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGoTestExcerptCapsTotalLines(t *testing.T) {
	t.Parallel()
	output := "--- FAIL: TestA (0.00s)\n    one\n    two\n--- FAIL: TestB (0.00s)\n    three\nFAIL\n"
	for _, tc := range []struct {
		maxLines int
		want     string
	}{
		{5, "--- FAIL: TestA (0.00s)\n    one\n    two\n--- FAIL: TestB (0.00s)\n    three"},
		{3, "--- FAIL: TestA (0.00s)\n    one\n... (3 lines cut)"},
		{1, "... (5 lines cut)"},
		{0, ""},
	} {
		if got := GoTestExcerpt(output, []string{"TestA", "TestB"}, tc.maxLines); got != tc.want {
			t.Errorf("maxLines %d: excerpt = %q, want %q", tc.maxLines, got, tc.want)
		}
	}
}
