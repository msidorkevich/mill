package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// gateReuseSetup is a pipeline whose gate appends a line to a counter file outside the
// worktrees each time it runs, then runs checkCmd. Every implementer writes the same
// hello.txt, so independent tasks end on the same tree; every verifier passes.
func gateReuseSetup(t *testing.T, checkCmd string, tasks int, tweak func(*config.Config)) (*env, string) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "runs")
	script := runner.Script{}
	for i := 0; i < tasks; i++ {
		script[runner.RoleImplementer] = append(script[runner.RoleImplementer], impl(map[string]string{"hello.txt": "hello\n"}))
		script[runner.RoleVerifier] = append(script[runner.RoleVerifier], verdict("pass"))
	}
	e := setup(t, "echo run >> '"+counter+"' && "+checkCmd, script, tweak)
	e.p.GateSlotPoll = time.Millisecond
	return e, counter
}

// gateReuseRuns is how often a gate command ran.
func gateReuseRuns(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run\n")
}

// gateReuseGateEvents are the data of the task's task-gate events.
func gateReuseGateEvents(t *testing.T, s *store.Store, id string) []map[string]any {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range evs {
		if ev.Kind != "task-gate" {
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

// gateReuseTree is the tree at the head of task id's worktree.
func gateReuseTree(t *testing.T, e *env, id string) string {
	t.Helper()
	br, err := e.p.Store.GetBranch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitx.HeadTree(context.Background(), br.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestGateReuseIdenticalTreeSkipsTheGateWithoutASlot(t *testing.T) {
	t.Parallel()
	e, counter := gateReuseSetup(t, "grep -q hello hello.txt", 2, nil)
	e.task(t, "a", "test -f hello.txt")
	e.task(t, "b", "test -f hello.txt")
	if out := e.run(t, "a"); out.Status != store.TaskVerified {
		t.Fatalf("a = %+v\n%s", out, e.out)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d times for a, want 1", n)
	}

	// Every gate slot is held: b only finishes because a reuse takes no slot.
	release := gateSlotPipelineHold(t, e)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := e.p.Run(ctx, "b")
	if err != nil || out.Status != store.TaskVerified {
		t.Fatalf("b = %+v, %v\n%s", out, err, e.out)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d times, want 1 (b reused a's pass)", n)
	}
	tree := gateReuseTree(t, e, "b")
	if tree != gateReuseTree(t, e, "a") {
		t.Fatal("a and b ended on different trees")
	}
	log := e.out.String()
	if want := "b a1 gate reused (tree " + tree + ")"; !strings.Contains(log, want) {
		t.Fatalf("log lacks %q:\n%s", want, log)
	}
	if strings.Contains(log, "waiting for a free gate slot") {
		t.Fatalf("the reused gate waited for a slot:\n%s", log)
	}
	evs := gateReuseGateEvents(t, e.p.Store, "b")
	if len(evs) != 1 || evs[0]["reused"] != true || evs[0]["tree"] != tree || evs[0]["reused_from"] != "a" {
		t.Fatalf("b gate events = %v", evs)
	}
	if res, _ := evs[0]["result"].(map[string]any); res["passed"] != true {
		t.Fatalf("reused gate result = %v", evs[0]["result"])
	}
	if a := gateReuseGateEvents(t, e.p.Store, "a"); len(a) != 1 || a[0]["reused"] != nil {
		t.Fatalf("a gate events = %v", a)
	}
}

func TestGateReuseChangedChecksRunTheGate(t *testing.T) {
	t.Parallel()
	e, counter := gateReuseSetup(t, "grep -q hello hello.txt", 2, nil)
	e.task(t, "a", "test -f hello.txt")
	e.task(t, "b", "test -f hello.txt", "test -f README")
	for _, id := range []string{"a", "b"} {
		if out := e.run(t, id); out.Status != store.TaskVerified {
			t.Fatalf("%s = %+v\n%s", id, out, e.out)
		}
	}
	if n := gateReuseRuns(t, counter); n != 2 {
		t.Fatalf("gate ran %d times, want 2 (b's checks differ)", n)
	}
	if strings.Contains(e.out.String(), "gate reused") {
		t.Fatalf("a gate with other checks was reused:\n%s", e.out)
	}
}

func TestGateReuseChangedCheckCmdRunsTheGate(t *testing.T) {
	t.Parallel()
	e, counter := gateReuseSetup(t, "grep -q hello hello.txt", 2, nil)
	e.task(t, "a")
	e.task(t, "b")
	if out := e.run(t, "a"); out.Status != store.TaskVerified {
		t.Fatalf("a = %+v\n%s", out, e.out)
	}
	e.p.Cfg.CheckCmd += " && true"
	if out := e.run(t, "b"); out.Status != store.TaskVerified {
		t.Fatalf("b = %+v\n%s", out, e.out)
	}
	if n := gateReuseRuns(t, counter); n != 2 {
		t.Fatalf("gate ran %d times, want 2 (check_cmd changed)", n)
	}
	if strings.Contains(e.out.String(), "gate reused") {
		t.Fatalf("a gate with another check_cmd was reused:\n%s", e.out)
	}
}

func TestGateReuseFailedGateIsNotReused(t *testing.T) {
	t.Parallel()
	e, counter := gateReuseSetup(t, "false", 2, func(c *config.Config) { c.MaxAttempts = 1 })
	e.task(t, "a")
	e.task(t, "b")
	if out := e.run(t, "a"); out.Status == store.TaskVerified {
		t.Fatalf("a passed a failing gate:\n%s", e.out)
	}
	afterA := gateReuseRuns(t, counter)
	if afterA == 0 {
		t.Fatal("a's gate never ran")
	}
	if out := e.run(t, "b"); out.Status == store.TaskVerified {
		t.Fatalf("b passed a failing gate:\n%s", e.out)
	}
	if n := gateReuseRuns(t, counter); n <= afterA {
		t.Fatalf("gate ran %d times in total, %d for a: b's gate did not run", n, afterA)
	}
	if strings.Contains(e.out.String(), "gate reused") {
		t.Fatalf("a failed gate was reused:\n%s", e.out)
	}
}
