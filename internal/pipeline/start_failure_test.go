package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// unstartableHost cannot start any command whose script contains marker ("" = every
// command); it starts the others on the exec host. starts counts every Start of a command
// it cannot start.
type unstartableHost struct {
	name   string
	marker string
	starts atomic.Int32
}

func (h *unstartableHost) Name() string { return h.name }

func (h *unstartableHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	if strings.Contains(strings.Join(spec.Argv, " "), h.marker) {
		h.starts.Add(1)
		return nil, errors.New("the wrapper never wrote its pid")
	}
	return host.NewExec().Start(ctx, spec)
}

func (e *env) gateResultRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.p.Store.DB().QueryRow(`SELECT COUNT(*) FROM gate_results`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartFailureRunsOnExecAndIsRecordedOnceWithNote(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, checkBase)
	h := &unstartableHost{name: "herdr"}
	e.p.Host = h
	e.task(t, "fallback")
	out := e.run(t, "fallback")
	if out.Status != store.TaskVerified || out.Attempts != 1 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("herdr tried to start the command %d time(s), want 2", got)
	}
	gates := e.events(t, "fallback", "task-gate")
	if len(gates) != 1 {
		t.Fatalf("%d task-gate events, want 1: %v", len(gates), gates)
	}
	res, _ := gates[0]["result"].(map[string]any)
	cmds, _ := res["commands"].([]any)
	c, _ := cmds[0].(map[string]any)
	if res["passed"] != true || c["exit"] != float64(0) || !strings.Contains(c["note"].(string), "ran on exec") {
		t.Fatalf("gate result = %v", res)
	}
	if n := e.gateResultRows(t); n != 1 {
		t.Fatalf("%d gate_results rows, want 1", n)
	}
	if len(e.events(t, "fallback", "gate-flaky")) != 0 {
		t.Fatal("a start failure was reported as flaky")
	}
}

func TestStartFailureIsNeverFlakyRerunOrBaseChecked(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, func(c *config.Config) { c.GateRerun, c.GateCheckBase, c.MaxAttempts = true, true, 1 })
	// The exec host itself cannot start it, so there is no host left to fall back to.
	h := &unstartableHost{name: "exec"}
	e.p.Host = h
	e.task(t, "unstartable")
	out := e.run(t, "unstartable")
	if out.Status == store.TaskVerified {
		t.Fatalf("outcome = %+v, want a failed attempt", out)
	}
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("the command was started %d time(s), want 2 (one retry, no triage re-run)", got)
	}
	for _, kind := range []string{"gate-flaky", "gate-base-failing", "gate-base-compared"} {
		if evs := e.events(t, "unstartable", kind); len(evs) != 0 {
			t.Fatalf("%s events = %v", kind, evs)
		}
	}
	for _, sub := range []string{"rerun", "base"} {
		if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "unstartable", "a1", sub)); !os.IsNotExist(err) {
			t.Fatalf("%s logs exist (err %v): triage ran the start failure", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), ".base", "unstartable")); !os.IsNotExist(err) {
		t.Fatalf("a base worktree was created for a start failure (err %v)", err)
	}
	if n := len(e.events(t, "unstartable", "task-gate")); n != 1 {
		t.Fatalf("%d task-gate events, want 1", n)
	}
	if n := e.gateResultRows(t); n != 0 {
		t.Fatalf("%d gate_results rows, want 0 for a failed gate", n)
	}
	f := e.events(t, "unstartable", "attempt-findings")
	if len(f) != 1 || !strings.Contains(strings.ToLower(jsonString(t, f[0])), "could not run") {
		t.Fatalf("findings = %v", f)
	}
}

func TestStartFailureBesideFlakyCommandTriagesOnlyTheCommandThatRan(t *testing.T) {
	t.Parallel()
	flaky := flakyCmd(filepath.Join(t.TempDir(), "count"))
	e := setup(t, flaky, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, func(c *config.Config) { c.GateRerun, c.GateCheckBase, c.MaxAttempts = true, true, 1 })
	h := &unstartableHost{name: "exec", marker: "UNSTARTABLE"}
	e.p.Host = h
	e.task(t, "mixed", "echo UNSTARTABLE")
	out := e.run(t, "mixed")
	if out.Status == store.TaskVerified {
		t.Fatalf("outcome = %+v, want a failed attempt", out)
	}
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("the unstartable command was started %d time(s), want 2", got)
	}
	evs := e.events(t, "mixed", "gate-flaky")
	if len(evs) != 1 {
		t.Fatalf("gate-flaky events = %v", evs)
	}
	if cmds, _ := evs[0]["commands"].([]any); len(cmds) != 1 || cmds[0] != flaky {
		t.Fatalf("gate-flaky commands = %v, want only the command that ran", evs[0]["commands"])
	}
	if evs := e.events(t, "mixed", "gate-base-failing"); len(evs) != 0 {
		t.Fatalf("gate-base-failing events = %v", evs)
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "mixed", "a1", "base")); !os.IsNotExist(err) {
		t.Fatalf("base check ran (err %v)", err)
	}
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
