package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/testenv"
)

// runScript writes a gate script whose output depends on how often it has run: run k runs
// runs[k-1], and runs past the end exit 0. It returns the gate command and the counter file.
func runScript(t *testing.T, runs ...string) (cmd, counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "count")
	body := "n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + counter + "'\ncase $n in\n"
	for k, r := range runs {
		body += strconv.Itoa(k+1) + ")\n" + r + "\n;;\n"
	}
	body += "esac\nexit 0\n"
	path := filepath.Join(dir, "gate.sh")
	testenv.WriteExecutable(t, path, []byte(body))
	return "sh '" + path + "'", counter
}

func goFails(tests ...string) string {
	s := ""
	for _, x := range tests {
		s += fail(x)
	}
	return s + goFailFooter
}

func assertRuns(t *testing.T, counter string, want string) {
	t.Helper()
	if b, _ := os.ReadFile(counter); strings.TrimSpace(string(b)) != want {
		t.Fatalf("gate command ran %q time(s), want %s", strings.TrimSpace(string(b)), want)
	}
}

func flakyTestsOf(t *testing.T, ev map[string]any, cmd string) []string {
	t.Helper()
	all, _ := ev["tests"].(map[string]any)
	list, ok := all[cmd].([]any)
	if !ok {
		t.Fatalf("no flaky tests for %q in event %v", cmd, ev)
	}
	var out []string
	for _, v := range list {
		out = append(out, v.(string))
	}
	return out
}

func oneAttempt(c *config.Config) { c.MaxAttempts = 1 }

func TestDisjointGoTestFailuresThenPassAreFlaky(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, goFails("TestA"), goFails("TestB"))
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, oneAttempt)
	e.task(t, "disjoint")
	out := e.run(t, "disjoint")
	if out.Status != store.TaskVerified || out.Attempts != 1 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "3")
	evs := e.events(t, "disjoint", "gate-flaky")
	if len(evs) != 1 {
		t.Fatalf("gate-flaky events = %v", evs)
	}
	if cmds, _ := evs[0]["commands"].([]any); len(cmds) != 1 || cmds[0] != cmd {
		t.Fatalf("gate-flaky commands = %v", evs[0]["commands"])
	}
	if got := flakyTestsOf(t, evs[0], cmd); !slices.Equal(got, []string{"TestA", "TestB"}) {
		t.Fatalf("flaky tests = %v", got)
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "disjoint", "a1", "rerun2", "gate-1.log")); err != nil {
		t.Fatalf("third run log: %v", err)
	}
}

func TestDisjointGoTestFailuresThenOnlyNewFailuresAreFlaky(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, goFails("TestA"), goFails("TestB"), goFails("TestC"))
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, oneAttempt)
	e.task(t, "newfail")
	out := e.run(t, "newfail")
	if out.Status != store.TaskVerified || out.Attempts != 1 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "3")
	evs := e.events(t, "newfail", "gate-flaky")
	if len(evs) != 1 {
		t.Fatalf("gate-flaky events = %v", evs)
	}
	if got := flakyTestsOf(t, evs[0], cmd); !slices.Equal(got, []string{"TestA", "TestB", "TestC"}) {
		t.Fatalf("flaky tests = %v", got)
	}
}

func TestGoTestFailingInTwoOfThreeRunsIsTheOnlyFinding(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, goFails("TestA"), goFails("TestB"), goFails("TestA", "TestC"))
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, oneAttempt)
	e.task(t, "twice")
	out := e.run(t, "twice")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "3")
	if evs := e.events(t, "twice", "gate-flaky"); len(evs) != 0 {
		t.Fatalf("gate-flaky recorded for a real failure: %v", evs)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "twice")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "failing in two of three runs: TestA.") ||
		!strings.Contains(f[0].Issue, "TestA broke") ||
		strings.Contains(f[0].Issue, "TestB") || strings.Contains(f[0].Issue, "TestC") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestOverlappingGoTestFailuresAreARealFailureWithoutThirdRun(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, goFails("TestA"), goFails("TestA", "TestB"))
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, oneAttempt)
	e.task(t, "overlap")
	out := e.run(t, "overlap")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "2")
	if evs := e.events(t, "overlap", "gate-flaky"); len(evs) != 0 {
		t.Fatalf("gate-flaky recorded for a real failure: %v", evs)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "overlap")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "`"+cmd+"` failed (exit 1)") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestNonGoCommandFailingTwiceIsARealFailure(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, "echo 'FAIL src/a.test.js'; exit 1", "echo 'FAIL src/b.test.js'; exit 1")
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, oneAttempt)
	e.task(t, "nongo2")
	out := e.run(t, "nongo2")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "2")
	if evs := e.events(t, "nongo2", "gate-flaky"); len(evs) != 0 {
		t.Fatalf("gate-flaky recorded for a real failure: %v", evs)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "nongo2")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "`"+cmd+"` failed (exit 1)") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestUnparseableThirdRunIsARealFailure(t *testing.T) {
	t.Parallel()
	cmd, counter := runScript(t, goFails("TestA"), goFails("TestB"), "echo 'segfault'; exit 1")
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, oneAttempt)
	e.task(t, "undecided")
	out := e.run(t, "undecided")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	assertRuns(t, counter, "3")
	if evs := e.events(t, "undecided", "gate-flaky"); len(evs) != 0 {
		t.Fatalf("gate-flaky recorded although the check could not decide: %v", evs)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "undecided")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "`"+cmd+"` failed (exit 1)") || !strings.Contains(f[0].Issue, "TestA") {
		t.Fatalf("findings = %+v", f)
	}
}
