package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mill/internal/config"
	"mill/internal/gitx"
	"mill/internal/runner"
	"mill/internal/store"
)

// flakyCmd fails the first time it runs and passes afterwards, counting runs in a file.
func flakyCmd(counter string) string {
	return `n=$(cat '` + counter + `' 2>/dev/null || echo 0); echo $((n+1)) > '` + counter + `'; [ "$n" -ge 1 ]`
}

// breakBase commits a file named broken on main, so `test ! -f broken` fails on the task's
// starting point and in its worktree alike.
func (e *env) breakBase(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.root, "broken"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(context.Background(), e.root, "break the base"); err != nil {
		t.Fatal(err)
	}
}

func (e *env) events(t *testing.T, id, kind string) []map[string]any {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range evs {
		if ev.Kind != kind {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// assertNoBaseWorktree checks that only main and the task worktree remain registered and
// nothing is left under the temporary base worktree directory.
func (e *env) assertNoBaseWorktree(t *testing.T, id string) {
	t.Helper()
	list, err := gitx.Run(context.Background(), e.root, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(list, "worktree "); n != 2 {
		t.Fatalf("%d worktrees registered, want main and the task's:\n%s", n, list)
	}
	ents, err := os.ReadDir(filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), ".base", id))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("base worktree directory left behind: %v", ents)
	}
}

func checkBase(c *config.Config) { c.GateCheckBase = true }

func TestFlakyGateCommandPassesOnRerunAndVerifierIsTold(t *testing.T) {
	cmd := flakyCmd(filepath.Join(t.TempDir(), "count"))
	e := setup(t, cmd, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, checkBase)
	e.task(t, "flaky")
	out := e.run(t, "flaky")
	if out.Status != store.TaskVerified || out.Attempts != 1 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	evs := e.events(t, "flaky", "gate-flaky")
	if len(evs) != 1 || evs[0]["attempt"] != float64(1) {
		t.Fatalf("gate-flaky events = %v", evs)
	}
	if cmds, _ := evs[0]["commands"].([]any); len(cmds) != 1 || cmds[0] != cmd {
		t.Fatalf("gate-flaky commands = %v", evs[0]["commands"])
	}
	vp := prompts(e.fake, runner.RoleVerifier)
	if len(vp) != 1 || !strings.Contains(vp[0], "## Flaky gate commands\n- `"+cmd+"`") ||
		!strings.Contains(vp[0], "failed once and passed on a re-run") || !strings.Contains(vp[0], "`"+cmd+"`: passed") {
		t.Fatalf("verifier prompt:\n%s", vp)
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "flaky", "a1", "rerun", "gate-1.log")); err != nil {
		t.Fatalf("re-run log: %v", err)
	}
	if len(e.events(t, "flaky", "gate-base-failing")) != 0 {
		t.Fatal("base check ran although the re-run passed")
	}
}

func TestGateFailingOnStartPointBlocksWithoutRetry(t *testing.T) {
	e := setup(t, "test ! -f broken", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}), impl(map[string]string{"hello.txt": "hello again\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, checkBase)
	e.breakBase(t)
	e.task(t, "based")
	out := e.run(t, "based")
	if out.Status != store.TaskBlocked || out.Attempts != 1 ||
		!strings.Contains(out.Reason, "`test ! -f broken`") ||
		!strings.Contains(out.Reason, "fail on the task's starting point") ||
		!strings.Contains(out.Reason, "this task did not cause them") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if n := len(prompts(e.fake, runner.RoleImplementer)); n != 1 {
		t.Fatalf("implementer ran %d time(s), want 1", n)
	}
	task, _ := e.p.Store.GetTask(context.Background(), "based")
	if task.Status != store.TaskBlocked || task.Phase != store.PhaseGate {
		t.Fatalf("task = %s (%s)", task.Status, task.Phase)
	}
	evs := e.events(t, "based", "gate-base-failing")
	if len(evs) != 1 || evs[0]["attempt"] != float64(1) {
		t.Fatalf("gate-base-failing events = %v", evs)
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "based", "a1", "base", "gate-1.log")); err != nil {
		t.Fatalf("base log: %v", err)
	}
	e.assertNoBaseWorktree(t, "based")
}

func TestGateFailingOnlyWithTaskChangesIsAFinding(t *testing.T) {
	e := setup(t, "! grep -q bad hello.txt 2>/dev/null", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "bad\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, checkBase)
	e.task(t, "caused")
	out := e.run(t, "caused")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if !strings.Contains(ip[1], "failed (exit 1)") || !strings.Contains(ip[1], "passes on the task's starting point") ||
		!strings.Contains(ip[1], "so the task's changes cause it") {
		t.Fatalf("second implementer prompt:\n%s", ip[1])
	}
	if len(e.events(t, "caused", "gate-base-failing")) != 0 {
		t.Fatal("gate-base-failing recorded for a failure the task caused")
	}
	e.assertNoBaseWorktree(t, "caused")
}

func TestGateFailuresSplitIntoCausedAndPreexisting(t *testing.T) {
	e := setup(t, "test ! -f broken", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "bad\n"})},
	}, func(c *config.Config) { checkBase(c); c.MaxAttempts = 1 })
	e.breakBase(t)
	e.task(t, "mixed", "! grep -q bad hello.txt 2>/dev/null")
	out := e.run(t, "mixed")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "mixed")
	if len(f) != 2 {
		t.Fatalf("findings = %+v", f)
	}
	if !strings.Contains(f[0].Issue, "grep -q bad") || !strings.Contains(f[0].Issue, "the task's changes cause it") {
		t.Fatalf("caused finding = %q", f[0].Issue)
	}
	if !strings.Contains(f[1].Issue, "`test ! -f broken` fails on the task's starting point") ||
		!strings.Contains(f[1].Issue, "not caused by this task") || !strings.Contains(f[1].Issue, "do not try to fix it") {
		t.Fatalf("preexisting finding = %q", f[1].Issue)
	}
	if len(e.events(t, "mixed", "gate-base-failing")) != 0 {
		t.Fatal("gate-base-failing recorded although the task caused a failure")
	}
}

func TestGateRerunOffSkipsRerun(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	e := setup(t, flakyCmd(counter), runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}), impl(map[string]string{"a": "2"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.GateRerun = false })
	e.task(t, "norerun")
	out := e.run(t, "norerun")
	// Attempt 1's gate runs once and fails; attempt 2's gate passes.
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if b, _ := os.ReadFile(counter); strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("gate ran %q time(s), want 2", b)
	}
	if len(e.events(t, "norerun", "gate-flaky")) != 0 {
		t.Fatal("gate-flaky recorded with gate_rerun = false")
	}
	if ip := prompts(e.fake, runner.RoleImplementer); !strings.Contains(ip[1], "failed (exit 1)") {
		t.Fatalf("second implementer prompt:\n%s", ip[1])
	}
}

func TestGateCheckBaseOffSkipsBaseCheck(t *testing.T) {
	e := setup(t, "test ! -f broken", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
	}, func(c *config.Config) { c.GateCheckBase = false; c.MaxAttempts = 1 })
	e.breakBase(t)
	e.task(t, "nobase")
	out := e.run(t, "nobase")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if len(e.events(t, "nobase", "gate-base-failing")) != 0 {
		t.Fatal("gate-base-failing recorded with gate_check_base = false")
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "nobase", "a1", "base")); !os.IsNotExist(err) {
		t.Fatalf("base check ran: %v", err)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "nobase")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "failed (exit 1)") || strings.Contains(f[0].Issue, "starting point") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestBaseWorktreeFailureFallsBackToGateFindings(t *testing.T) {
	e := setup(t, "test ! -f broken", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
	}, func(c *config.Config) { checkBase(c); c.MaxAttempts = 1 })
	e.breakBase(t)
	// A file where the base worktrees go makes creating one fail.
	wts := e.p.Cfg.Worktrees(e.root, e.p.Home)
	if err := os.MkdirAll(wts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wts, ".base"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.task(t, "fallback")
	out := e.run(t, "fallback")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if !strings.Contains(e.out.String(), "warning: could not check the failing commands on the starting point") {
		t.Fatalf("no warning logged:\n%s", e.out)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "fallback")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "`test ! -f broken` failed (exit 1)") || strings.Contains(f[0].Issue, "starting point") {
		t.Fatalf("findings = %+v", f)
	}
	if len(e.events(t, "fallback", "gate-base-failing")) != 0 {
		t.Fatal("gate-base-failing recorded without a base check")
	}
}

func TestBaseWorktreeCreationWaitsForGitLock(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	e := setup(t, "pwd >> '"+calls+"'; test ! -f broken", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
	}, checkBase)
	e.breakBase(t)
	e.task(t, "locked")
	countCalls := func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "\n")
	}

	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	// prepare takes the git lock itself; the implementer run waits until the test holds it.
	wr := &waitRunner{Runner: e.p.Runner, wait: make(chan struct{}), started: make(chan struct{}, 1)}
	e.p.Runner = wr
	go func() {
		out, err := e.p.Run(context.Background(), "locked")
		done <- result{out, err}
	}()
	<-wr.started
	e.p.gitMu.Lock()
	locked := true
	defer func() {
		if locked {
			e.p.gitMu.Unlock()
		}
	}()
	close(wr.wait)

	// The gate and its re-run happen without the lock.
	deadline := time.Now().Add(10 * time.Second)
	for countCalls() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("gate did not run twice:\n%s", e.out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give an unserialized implementation the chance to create the base worktree and run
	// the command there; with the lock held it must not.
	time.Sleep(300 * time.Millisecond)
	if n := countCalls(); n != 2 {
		t.Fatalf("base check ran while the git lock was held (%d gate runs)", n)
	}
	if list, _ := gitx.Run(context.Background(), e.root, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 2 {
		t.Fatalf("base worktree created while the git lock was held:\n%s", list)
	}
	locked = false
	e.p.gitMu.Unlock()

	r := <-done
	if r.err != nil || r.out.Status != store.TaskBlocked || !strings.Contains(r.out.Reason, "did not cause them") {
		t.Fatalf("outcome = %+v, %v\n%s", r.out, r.err, e.out)
	}
	if n := countCalls(); n != 3 {
		t.Fatalf("gate runs = %d, want 3 (gate, re-run, base)", n)
	}
	e.assertNoBaseWorktree(t, "locked")
}

// waitRunner signals when an agent run starts and holds it until wait is closed.
type waitRunner struct {
	runner.Runner
	wait    chan struct{}
	started chan struct{}
}

func (w *waitRunner) Run(ctx context.Context, r runner.Request) (runner.Result, error) {
	w.started <- struct{}{}
	<-w.wait
	return w.Runner.Run(ctx, r)
}
