package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/testenv"
)

// goGateScript writes a shell script printing go-test-style output and returns the gate
// command that runs it. The script decides what fails from hello.txt, which the task
// writes and the starting point lacks.
func goGateScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.sh")
	testenv.WriteExecutable(t, path, []byte(body))
	return "sh '" + path + "'"
}

func fail(test string) string {
	return `printf -- '--- FAIL: ` + test + ` (0.00s)\n    x_test.go:1: ` + test + ` broke\n'` + "\n"
}

const goFailFooter = `printf 'FAIL\nFAIL\tx/pkg\t0.01s\nFAIL\n'; exit 1` + "\n"

// testsOf returns the tests map entry of cmd in an event, as string slices.
func testsOf(t *testing.T, ev map[string]any, cmd string) (head, base, headOnly []string) {
	t.Helper()
	all, _ := ev["tests"].(map[string]any)
	entry, ok := all[cmd].(map[string]any)
	if !ok {
		t.Fatalf("no tests for %q in event %v", cmd, ev)
	}
	strs := func(k string) []string {
		var out []string
		for _, v := range entry[k].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	return strs("head"), strs("base"), strs("head_only")
}

func TestGoTestFailingOnlyOnHeadIsCausedByTask(t *testing.T) {
	t.Parallel()
	cmd := goGateScript(t, "grep -q fixed hello.txt 2>/dev/null && exit 0\n"+
		"grep -q bad hello.txt 2>/dev/null && "+fail("TestA")+fail("TestFlakyB")+goFailFooter)
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "bad\n"}), impl(map[string]string{"hello.txt": "fixed\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, checkBase)
	e.task(t, "headonly")
	out := e.run(t, "headonly")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if len(ip) != 2 {
		t.Fatalf("implementer ran %d time(s), want 2", len(ip))
	}
	p := ip[1]
	if !strings.Contains(p, "failing only on the task's head: TestA.") ||
		!strings.Contains(p, "so the task's changes cause them") ||
		!strings.Contains(p, "TestA broke") ||
		!strings.Contains(p, "Also failing on the starting point") ||
		!strings.Contains(p, "not your fault; do not try to fix them: TestFlakyB") {
		t.Fatalf("second implementer prompt:\n%s", p)
	}
	if len(e.events(t, "headonly", "gate-base-failing")) != 0 {
		t.Fatal("gate-base-failing recorded although the task caused a failure")
	}
	evs := e.events(t, "headonly", "gate-base-compared")
	if len(evs) != 1 || evs[0]["attempt"] != float64(1) || evs[0]["start"] == "" {
		t.Fatalf("gate-base-compared events = %v", evs)
	}
	head, base, headOnly := testsOf(t, evs[0], cmd)
	if !slices.Equal(head, []string{"TestA", "TestFlakyB"}) || !slices.Equal(base, []string{"TestFlakyB"}) ||
		!slices.Equal(headOnly, []string{"TestA"}) {
		t.Fatalf("tests = head %v, base %v, head_only %v", head, base, headOnly)
	}
	e.assertNoBaseWorktree(t, "headonly")
}

func TestGoTestSameFailuresOnBaseBlock(t *testing.T) {
	t.Parallel()
	cmd := goGateScript(t, fail("TestB")+goFailFooter)
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, checkBase)
	e.task(t, "same")
	out := e.run(t, "same")
	if out.Status != store.TaskBlocked || out.Attempts != 1 ||
		!strings.Contains(out.Reason, "fail on the task's starting point") ||
		!strings.Contains(out.Reason, "this task did not cause them") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	evs := e.events(t, "same", "gate-base-failing")
	if len(evs) != 1 {
		t.Fatalf("gate-base-failing events = %v", evs)
	}
	head, base, headOnly := testsOf(t, evs[0], cmd)
	if !slices.Equal(head, []string{"TestB"}) || !slices.Equal(base, []string{"TestB"}) || len(headOnly) != 0 {
		t.Fatalf("tests = head %v, base %v, head_only %v", head, base, headOnly)
	}
	if len(e.events(t, "same", "gate-base-compared")) != 0 {
		t.Fatal("gate-base-compared recorded although nothing was caused here")
	}
}

func TestNonGoTestOutputFailingOnBaseBlocksWholeCommand(t *testing.T) {
	t.Parallel()
	cmd := goGateScript(t, "grep -q bad hello.txt 2>/dev/null && echo 'npm ERR! bad'\necho 'FAIL src/a.test.js'; exit 1\n")
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "bad\n"})},
	}, checkBase)
	e.task(t, "nongo")
	out := e.run(t, "nongo")
	if out.Status != store.TaskBlocked || out.Attempts != 1 || !strings.Contains(out.Reason, "this task did not cause them") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	evs := e.events(t, "nongo", "gate-base-failing")
	if len(evs) != 1 {
		t.Fatalf("gate-base-failing events = %v", evs)
	}
	if _, ok := evs[0]["tests"]; ok {
		t.Fatalf("tests recorded for a whole-command comparison: %v", evs[0])
	}
}

func TestGoTestBuildFailureOnlyOnHeadIsCausedByTask(t *testing.T) {
	t.Parallel()
	cmd := goGateScript(t, `if grep -q bad hello.txt 2>/dev/null; then
printf '# x/pkg\n./a.go:3:2: undefined: foo\nFAIL\tx/pkg [build failed]\nFAIL\n'; exit 1
fi
`+fail("TestOther")+goFailFooter)
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "bad\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
	}, checkBase)
	e.task(t, "build")
	out := e.run(t, "build")
	// Attempt 1 broke the build (caused here, retried); attempt 2 only fails TestOther,
	// which fails on the starting point too.
	if out.Status != store.TaskBlocked || out.Attempts != 2 || !strings.Contains(out.Reason, "this task did not cause them") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if len(ip) != 2 || !strings.Contains(ip[1], "failing only on the task's head: pkg:x/pkg.") ||
		!strings.Contains(ip[1], "undefined: foo") ||
		!strings.Contains(ip[1], "not your fault; do not try to fix them: TestOther") {
		t.Fatalf("implementer prompts:\n%s", ip)
	}
	evs := e.events(t, "build", "gate-base-compared")
	if len(evs) != 1 || evs[0]["attempt"] != float64(1) {
		t.Fatalf("gate-base-compared events = %v", evs)
	}
	if _, _, headOnly := testsOf(t, evs[0], cmd); !slices.Equal(headOnly, []string{"pkg:x/pkg"}) {
		t.Fatalf("head_only = %v", headOnly)
	}
	if evs := e.events(t, "build", "gate-base-failing"); len(evs) != 1 || evs[0]["attempt"] != float64(2) {
		t.Fatalf("gate-base-failing events = %v", evs)
	}
}

func TestUnreadableGoTestLogFallsBackToWholeCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "gate-1.log")
	if err := os.WriteFile(log, []byte("--- FAIL: TestA (0.00s)\nFAIL\tx/pkg\t0.01s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.log")
	if _, ok := compareGoTests(log, missing); ok {
		t.Fatal("compared by tests with an unreadable base log")
	}
	if _, ok := compareGoTests(missing, log); ok {
		t.Fatal("compared by tests with an unreadable head log")
	}
	if cmp, ok := compareGoTests(log, log); !ok || len(cmp.headOnly) != 0 {
		t.Fatalf("same logs: %+v, %v", cmp, ok)
	}
}
