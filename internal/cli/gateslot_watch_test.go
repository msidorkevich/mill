package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// gateSlotPipelineRepo is schedRepo with max_parallel = 2 and max_parallel_gates = 1.
func gateSlotPipelineRepo(t *testing.T, script string) string {
	t.Helper()
	root := schedRepo(t, script, 2)
	path := project.PathsAt(root).Config
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(cfg), "max_parallel_gates = 2", "max_parallel_gates = 1", 1)
	if updated == string(cfg) {
		t.Fatalf("config has no max_parallel_gates = 2 to lower:\n%s", cfg)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// gateSlotPipelineRow returns the watch row of task id, or "".
func gateSlotPipelineRow(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == id {
			return line
		}
	}
	return ""
}

func TestGateSlotWatchShowsTaskWaitingForGate(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	root := gateSlotPipelineRepo(t, schedScript(map[string]string{"g1/a": "a.txt", "g1/b": "b.txt"}))
	approvePlan(t, root, "g1", schedPlan(t, g, "Gates take turns",
		schedTask{id: "a", file: "a.txt", barrier: "gate"}, schedTask{id: "b", file: "b.txt", barrier: "gate"}))
	s := openTestStore(t, root)
	r := startRun(t, root, g)

	// Both agents ran; one task holds the only gate slot at its barrier, the other waits.
	var holder, waiter string
	waitFor(t, "one task in its gate and the other waiting for a gate slot", func() bool {
		phases := map[string]string{}
		for _, id := range []string{"g1/a", "g1/b"} {
			st, ph := statusOf(t, s, id)
			if st != store.TaskRunning {
				return false
			}
			phases[ph] = id
		}
		holder, waiter = phases[store.PhaseGate], phases[store.PhaseGateWait]
		return holder != "" && waiter != ""
	})

	w := run(t, root, nil, "watch", "--once", "--interval", groupBWatchInterval)
	if w.code != ExitOK || strings.Contains(w.stdout, "nothing running") {
		t.Fatalf("watch --once: %+v", w)
	}
	if row := gateSlotPipelineRow(w.stdout, waiter); !strings.Contains(row, "waiting for gate") || !strings.HasSuffix(strings.TrimSpace(row), " -") {
		t.Errorf("watch row of %s = %q, want stage `waiting for gate` and no detail:\n%s", waiter, row, w.stdout)
	}
	if row := gateSlotPipelineRow(w.stdout, holder); strings.Fields(row + " x x x")[2] != store.PhaseGate {
		t.Errorf("watch row of %s = %q, want stage gate:\n%s", holder, row, w.stdout)
	}

	g.release(t, "gate")
	if code := r.finish(t); code != ExitOK {
		t.Fatalf("mill run exit %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
	for _, id := range []string{"g1/a", "g1/b"} {
		if st, _ := statusOf(t, s, id); st != store.TaskVerified {
			t.Errorf("%s = %s, want verified\n%s", id, st, r.stdout)
		}
	}
}

func TestGateSlotWatchStageWaitingForGateHasNoGateLogDetail(t *testing.T) {
	t.Parallel()
	root, at := stageRepo(t, store.PhaseGateWait)
	paths := project.PathsAt(root)
	s, err := store.Open(context.Background(), paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	if err := renderWatch(context.Background(), &b, paths, s, "", at); err != nil {
		t.Fatal(err)
	}
	row := gateSlotPipelineRow(b.String(), "g1/a")
	if !strings.Contains(row, "waiting for gate") || strings.Contains(row, "gate log") || !strings.HasSuffix(strings.TrimSpace(row), " -") {
		t.Fatalf("watch row = %q, want stage `waiting for gate` and detail -:\n%s", row, b.String())
	}
}
