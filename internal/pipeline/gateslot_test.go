package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

const gateSlotPipelinePoll = time.Millisecond

// gateSlotPipelineSetup is setup with millisecond gate slot polls.
func gateSlotPipelineSetup(t *testing.T, checkCmd string, script runner.Script, tweak func(*config.Config)) *env {
	t.Helper()
	e := setup(t, checkCmd, script, tweak)
	e.p.GateSlotPoll = gateSlotPipelinePoll
	return e
}

// gateSlotPipelineHold takes a gate slot as another holder would; release it with the
// returned func.
func gateSlotPipelineHold(t *testing.T, e *env) func() {
	t.Helper()
	release, err := e.p.Store.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "other", Attempt: 1, PID: os.Getpid()},
		1, gateSlotPipelinePoll, func() { t.Error("the test's own slot had to wait: a slot was left held") })
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
}

// gateSlotPipelineWaitPhase polls until task id is running in phase, or fails after a deadline.
func gateSlotPipelineWaitPhase(t *testing.T, e *env, id, phase string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		tk, err := e.p.Store.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Status == store.TaskRunning && tk.Phase == phase {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached phase %q (now %s/%q)", id, phase, tk.Status, tk.Phase)
		}
		time.Sleep(gateSlotPipelinePoll)
	}
}

// gateSlotPipelineWaitAnyPhase polls until one of ids is running in phase, or fails after a
// deadline.
func gateSlotPipelineWaitAnyPhase(t *testing.T, e *env, ids []string, phase string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		for _, id := range ids {
			tk, err := e.p.Store.GetTask(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tk.Status == store.TaskRunning && tk.Phase == phase {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("none of %v reached phase %q", ids, phase)
		}
		time.Sleep(gateSlotPipelinePoll)
	}
}

type gateSlotPipelineResult struct {
	out Outcome
	err error
}

func gateSlotPipelineStart(e *env, ctx context.Context, id string) chan gateSlotPipelineResult {
	done := make(chan gateSlotPipelineResult, 1)
	go func() {
		out, err := e.p.Run(ctx, id)
		done <- gateSlotPipelineResult{out, err}
	}()
	return done
}

func gateSlotPipelineWait(t *testing.T, done chan gateSlotPipelineResult) gateSlotPipelineResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the task run did not end")
		return gateSlotPipelineResult{}
	}
}

// gateSlotPipelineRendezvous holds each implementer run until n of them are running at once,
// which proves the agents run concurrently; it fails the run when they never meet.
type gateSlotPipelineRendezvous struct {
	inner   runner.Runner
	n       int
	mu      sync.Mutex
	arrived int
	all     chan struct{}
}

func (r *gateSlotPipelineRendezvous) Name() string { return "rendezvous" }
func (r *gateSlotPipelineRendezvous) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	if req.Role == runner.RoleImplementer {
		r.mu.Lock()
		r.arrived++
		if r.arrived == r.n {
			close(r.all)
		}
		r.mu.Unlock()
		select {
		case <-r.all:
		case <-time.After(20 * time.Second):
			return runner.Result{}, errors.New("implementers did not run concurrently")
		}
	}
	return r.inner.Run(ctx, req)
}

func TestGateSlotGatesOfConcurrentTasksNeverOverlapWhileAgentsDo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	record := filepath.Join(dir, "gates")
	open := filepath.Join(dir, "open")
	// Each gate records its start and end; overlapping gates would interleave the lines. A gate
	// holds its slot until the test sees the other task waiting for one.
	check := "echo start >> '" + record + "'; while [ ! -e '" + open + "' ]; do sleep 0.01; done; echo end >> '" + record + "'"
	step := func(file string) []runner.Step {
		return []runner.Step{{Files: map[string]string{file: "x\n"}, Output: json.RawMessage(`{"summary":"s","files_changed":[],"notes":""}`)}}
	}
	pass := []runner.Step{verdict("pass")}
	// The default config: max_parallel_gates = 1.
	e := gateSlotPipelineSetup(t, check, runner.Script{}, func(c *config.Config) { c.MaxParallel = 2 })
	if e.p.Cfg.MaxParallelGates != 1 {
		t.Fatalf("default max_parallel_gates = %d, want 1", e.p.Cfg.MaxParallelGates)
	}
	e.fake = runner.NewFake(runner.FakeScript{Tasks: map[string]runner.Script{
		"g1/a": {runner.RoleImplementer: step("a.txt"), runner.RoleVerifier: pass},
		"g1/b": {runner.RoleImplementer: step("b.txt"), runner.RoleVerifier: pass},
	}})
	e.p.Runner = &gateSlotPipelineRendezvous{inner: e.fake, n: 2, all: make(chan struct{})}
	e.goal(t, []string{}, []string{})

	ids := []string{"g1/a", "g1/b"}
	var runs []chan gateSlotPipelineResult
	for _, id := range ids {
		runs = append(runs, gateSlotPipelineStart(e, context.Background(), id))
	}
	gateSlotPipelineWaitAnyPhase(t, e, ids, store.PhaseGateWait)
	if err := os.WriteFile(open, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i, done := range runs {
		r := gateSlotPipelineWait(t, done)
		if r.err != nil || r.out.Status != store.TaskVerified {
			t.Fatalf("%s: %+v, %v\n%s", ids[i], r.out, r.err, e.out)
		}
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); strings.Join(got, " ") != "start end start end" {
		t.Fatalf("gate start/end = %v, want two gates one after the other", got)
	}
	if !strings.Contains(e.out.String(), "waiting for a free gate slot") {
		t.Errorf("no gate waited for a slot:\n%s", e.out)
	}
	gateSlotPipelineHold(t, e)()
}

func TestGateSlotWaitingTaskIsInWaitPhaseThenGate(t *testing.T) {
	t.Parallel()
	e := gateSlotPipelineSetup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "waits")
	release := gateSlotPipelineHold(t, e)
	done := gateSlotPipelineStart(e, context.Background(), "waits")
	gateSlotPipelineWaitPhase(t, e, "waits", store.PhaseGateWait)
	if evs := e.events(t, "waits", "task-gate"); len(evs) != 0 {
		t.Fatalf("the gate ran while every slot was held: %v", evs)
	}
	release()
	r := gateSlotPipelineWait(t, done)
	if r.err != nil || r.out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v, %v\n%s", r.out, r.err, e.out)
	}
	if evs := e.events(t, "waits", "task-gate"); len(evs) != 1 {
		t.Fatalf("task-gate events = %v", evs)
	}
	gateSlotPipelineHold(t, e)()
}

func TestGateSlotIsReleasedAfterFailingGate(t *testing.T) {
	t.Parallel()
	e := gateSlotPipelineSetup(t, "false", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, func(c *config.Config) { c.MaxAttempts = 1; c.GateRerun = true })
	e.task(t, "red")
	out := e.run(t, "red")
	if out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	gateSlotPipelineHold(t, e)()
}

func TestGateSlotIsReleasedWhenCancelledDuringGate(t *testing.T) {
	t.Parallel()
	never := filepath.Join(t.TempDir(), "never")
	e := gateSlotPipelineSetup(t, "while [ ! -e '"+never+"' ]; do sleep 0.01; done", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, nil)
	e.task(t, "cut")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := gateSlotPipelineStart(e, ctx, "cut")
	gateSlotPipelineWaitPhase(t, e, "cut", store.PhaseGate)
	cancel()
	r := gateSlotPipelineWait(t, done)
	if r.err != nil || r.out.Status != store.TaskBlocked || r.out.Reason != "interrupted" {
		t.Fatalf("outcome = %+v, %v\n%s", r.out, r.err, e.out)
	}
	gateSlotPipelineHold(t, e)()
}

func TestGateSlotWaitEndsWhenCancelled(t *testing.T) {
	t.Parallel()
	e := gateSlotPipelineSetup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
	}, nil)
	e.task(t, "queued")
	release := gateSlotPipelineHold(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := gateSlotPipelineStart(e, ctx, "queued")
	gateSlotPipelineWaitPhase(t, e, "queued", store.PhaseGateWait)
	cancel()
	r := gateSlotPipelineWait(t, done)
	if r.err != nil || r.out.Status != store.TaskBlocked || r.out.Reason != "interrupted" {
		t.Fatalf("outcome = %+v, %v\n%s", r.out, r.err, e.out)
	}
	if evs := e.events(t, "queued", "task-gate"); len(evs) != 0 {
		t.Fatalf("the gate ran although the wait was cancelled: %v", evs)
	}
	release()
	gateSlotPipelineHold(t, e)()
}
