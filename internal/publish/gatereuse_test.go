package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/store"
)

// gateReuseFixture is fixture with a check_cmd that appends a line to a counter file outside
// the worktree each time the gate runs, and a buffer for progress lines.
func gateReuseFixture(t *testing.T) (Options, store.Task, store.Branch, string, *bytes.Buffer) {
	t.Helper()
	o, _, br := fixture(t)
	counter := filepath.Join(t.TempDir(), "runs")
	o.Cfg.CheckCmd = "echo run >> '" + counter + "'"
	var out bytes.Buffer
	o.Out = &out
	tk, err := o.Store.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return o, tk, br, counter, &out
}

// gateReuseRuns is how often the gate ran.
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

// gateReuseLastGateEvent is the data of the task's last task-gate event.
func gateReuseLastGateEvent(t *testing.T, s *store.Store, id string) map[string]any {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "task-gate" {
			var d map[string]any
			if err := json.Unmarshal(evs[i].Data, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatal("no task-gate event")
	return nil
}

// gateReuseRestack moves the branch to a new commit with the same tree, as a restack onto an
// unchanged parent does.
func gateReuseRestack(t *testing.T, o Options, br store.Branch) store.Branch {
	t.Helper()
	git(t, br.Worktree, "commit", "-q", "--allow-empty", "-m", "restacked")
	br.HeadSHA = git(t, br.Worktree, "rev-parse", "HEAD")
	if err := o.Store.SaveBranch(context.Background(), br); err != nil {
		t.Fatal(err)
	}
	return br
}

func TestGateReuseRestackedTreeSkipsTheGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, tk, br, counter, out := gateReuseFixture(t)
	tree := git(t, br.Worktree, "rev-parse", "HEAD^{tree}")

	g, err := GateAtHead(ctx, o, tk, br, "sync")
	if err != nil || !g.Passed {
		t.Fatalf("first gate = %+v, %v", g, err)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d times, want 1", n)
	}
	if strings.Contains(out.String(), "gate reused") {
		t.Fatalf("first gate reported a reuse: %q", out)
	}
	if d := gateReuseLastGateEvent(t, o.Store, "t1"); d["reused"] != nil || d["tree"] != tree {
		t.Fatalf("first gate event = %v", d)
	}

	br = gateReuseRestack(t, o, br)
	g, err = GateAtHead(ctx, o, tk, br, "sync")
	if err != nil || !g.Passed || len(g.Commands) != 2 {
		t.Fatalf("reused gate = %+v, %v; want the stored pass", g, err)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d times, want 1 (reused)", n)
	}
	if want := "t1: gate reused (tree " + tree + ")"; !strings.Contains(out.String(), want) {
		t.Fatalf("output %q lacks %q", out, want)
	}
	d := gateReuseLastGateEvent(t, o.Store, "t1")
	if d["reused"] != true || d["tree"] != tree || d["head"] != br.HeadSHA || d["reused_from"] != "t1" || d["sync"] != true {
		t.Fatalf("reused gate event = %v", d)
	}
}

func TestGateReusePublishReusesThePassAtTheRestackedHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, tk, br, counter, out := gateReuseFixture(t)
	if _, err := GateAtHead(ctx, o, tk, br, "sync"); err != nil {
		t.Fatal(err)
	}
	br = gateReuseRestack(t, o, br) // gate_sha now differs from the head
	res, err := Publish(ctx, o)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.GateRerun == nil || !res.GateRerun.Passed {
		t.Fatalf("GateRerun = %+v, want the reused pass", res.GateRerun)
	}
	if n := gateReuseRuns(t, counter); n != 1 {
		t.Fatalf("gate ran %d times, want 1 (reused)", n)
	}
	if !strings.Contains(out.String(), "gate reused (tree ") {
		t.Fatalf("output %q lacks the reuse", out)
	}
	if after, _ := o.Store.GetBranch(ctx, "t1"); after.GateSHA != br.HeadSHA {
		t.Fatalf("gate_sha = %s, want %s", after.GateSHA, br.HeadSHA)
	}
}

func TestGateReuseChangedCheckCmdOrChecksRunsTheGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, tk, br, counter, out := gateReuseFixture(t)
	if _, err := GateAtHead(ctx, o, tk, br, "sync"); err != nil {
		t.Fatal(err)
	}

	o2 := o
	o2.Cfg.CheckCmd = o.Cfg.CheckCmd + " && true"
	if g, err := GateAtHead(ctx, o2, tk, br, "sync"); err != nil || !g.Passed {
		t.Fatalf("gate with a changed check_cmd = %+v, %v", g, err)
	}
	if n := gateReuseRuns(t, counter); n != 2 {
		t.Fatalf("gate ran %d times after check_cmd changed, want 2", n)
	}

	tk2 := tk
	tk2.Checks = append([]string{"test -f README"}, tk.Checks...)
	if g, err := GateAtHead(ctx, o, tk2, br, "sync"); err != nil || !g.Passed {
		t.Fatalf("gate with a changed check = %+v, %v", g, err)
	}
	if n := gateReuseRuns(t, counter); n != 3 {
		t.Fatalf("gate ran %d times after a check changed, want 3", n)
	}
	if strings.Contains(out.String(), "gate reused") {
		t.Fatalf("a changed gate was reused: %q", out)
	}
}

func TestGateReuseFailedGateIsNotReused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, tk, br, counter, out := gateReuseFixture(t)
	o.Cfg.CheckCmd += " && false"
	for i := 1; i <= 2; i++ {
		g, err := GateAtHead(ctx, o, tk, br, "sync")
		if err != nil || g.Passed {
			t.Fatalf("gate %d = %+v, %v; want a failure", i, g, err)
		}
		if n := gateReuseRuns(t, counter); n != i {
			t.Fatalf("gate ran %d times, want %d", n, i)
		}
	}
	if strings.Contains(out.String(), "gate reused") {
		t.Fatalf("a failed gate was reused: %q", out)
	}
	tree := git(t, br.Worktree, "rev-parse", "HEAD^{tree}")
	if _, ok, err := o.Store.PassedGate(ctx, tree, store.GateFingerprint(o.Cfg.CheckCmd, tk.Checks)); err != nil || ok {
		t.Fatalf("PassedGate after a failure = %v, %v; want a miss", ok, err)
	}
}
