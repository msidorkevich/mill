package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// implStep is an implementer step that writes file with content.
func implStep(file, content string) runner.Step {
	return runner.Step{
		Output: json.RawMessage(`{"summary":"wrote ` + file + `","files_changed":[],"notes":""}`),
		Files:  map[string]string{file: content},
	}
}

// passStep is a verifier step that passes.
func passStep() runner.Step {
	return runner.Step{Output: json.RawMessage(`{"verdict":"pass","summary":"ok","findings":[]}`)}
}

// heldRun runs tasks with run, except that each task in gates first waits for its channel to
// be closed (or for ctx to end).
func heldRun(run func(context.Context, string) (pipeline.Outcome, error), gates map[string]chan struct{}) func(context.Context, string) (pipeline.Outcome, error) {
	return func(ctx context.Context, id string) (pipeline.Outcome, error) {
		if g, ok := gates[id]; ok {
			select {
			case <-g:
			case <-ctx.Done():
			}
		}
		return run(ctx, id)
	}
}

// waitTaskStatus polls the store until the task has want, failing after a deadline.
func waitTaskStatus(t *testing.T, s *store.Store, id, want string) store.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		tk := status(t, s, id)
		if tk.Status == want {
			return tk
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s = %s, want %s", id, tk.Status, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func implementerAttempts(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	as, err := s.Attempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range as {
		if a.Role == string(runner.RoleImplementer) {
			n++
		}
	}
	return n
}

func headOf(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	br, err := s.GetBranch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return br.HeadSHA
}

func TestReopenedTaskRunsAgainInSameRunAndEndsVerified(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"}, task{id: "h"})
	a, h := store.TaskID(g, "a"), store.TaskID(g, "h")
	script := runner.FakeScript{Tasks: map[string]runner.Script{
		a: {
			runner.RoleImplementer: {implStep("a.go", "package a\n"), implStep("a.go", "package a\n\n// v2\n")},
			runner.RoleVerifier:    {passStep(), passStep()},
		},
		h: {
			runner.RoleImplementer: {implStep("h.go", "package h\n")},
			runner.RoleVerifier:    {passStep()},
		},
	}}
	out := &syncBuf{}
	p := newPipeline(t, s, script, out)
	gateH := make(chan struct{})
	// h waits until a has been reopened, so the run is still going when a returns to pending.
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: heldRun(p.Run, map[string]chan struct{}{h: gateH}),
		Out: out, Sleep: pollSleep}
	ch := runAsync(context.Background(), sch, g, Options{})

	waitTaskStatus(t, s, a, store.TaskVerified)
	firstHead := headOf(t, s, a)
	if err := s.RequestChanges(context.Background(), a, 1, []prompt.Finding{{Source: "operator", Issue: "redo"}}, "changes requested"); err != nil {
		t.Fatal(err)
	}
	close(gateH)

	r := wait(t, ch)
	log := out.String()
	if r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v, %v\n%s", r.res, r.err, log)
	}
	if got := status(t, s, a).Status; got != store.TaskVerified {
		t.Fatalf("%s = %s, want verified\n%s", a, got, log)
	}
	if n := implementerAttempts(t, s, a); n != 2 {
		t.Errorf("%s has %d implementer attempts, want 2 (one per run)\n%s", a, n, log)
	}
	if headOf(t, s, a) == firstHead {
		t.Errorf("%s head did not move after its second run\n%s", a, log)
	}
	if n := strings.Count(log, a+": started"); n != 2 {
		t.Errorf("%s started %d times, want 2\n%s", a, n, log)
	}
	if strings.Contains(log, "stuck") || strings.Contains(r.res.Reason, "stuck") {
		t.Errorf("a reopened task was reported stuck: %q\n%s", r.res.Reason, log)
	}
}

func TestDependentOfReopenedTaskWaitsForItToBeVerifiedAgain(t *testing.T) {
	t.Parallel()
	// b depends on a and shares an owned file with h, so b cannot start while h runs: a is
	// verified, then reopened before b starts.
	s, g := newGoal(t,
		task{id: "h", owns: []string{"h.go", "shared.go"}},
		task{id: "a"},
		task{id: "b", owns: []string{"b.go", "shared.go"}, deps: []string{"a"}})
	a, b, h := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "h")
	script := runner.FakeScript{Tasks: map[string]runner.Script{
		a: {
			runner.RoleImplementer: {implStep("a.go", "package a\n"), implStep("a.go", "package a\n\n// v2\n")},
			runner.RoleVerifier:    {passStep(), passStep()},
		},
		b: {
			runner.RoleImplementer: {implStep("b.go", "package b\n")},
			runner.RoleVerifier:    {passStep()},
		},
		h: {
			runner.RoleImplementer: {implStep("h.go", "package h\n")},
			runner.RoleVerifier:    {passStep()},
		},
	}}
	out := &syncBuf{}
	p := newPipeline(t, s, script, out)
	gateH := make(chan struct{})
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: heldRun(p.Run, map[string]chan struct{}{h: gateH}),
		Out: out, Sleep: pollSleep}
	ch := runAsync(context.Background(), sch, g, Options{})

	waitTaskStatus(t, s, a, store.TaskVerified)
	firstHead := headOf(t, s, a)
	if err := s.RequestChanges(context.Background(), a, 1, []prompt.Finding{{Source: "operator", Issue: "redo"}}, "changes requested"); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, b).Status; got != store.TaskPending {
		t.Fatalf("%s = %s before h finished, want pending", b, got)
	}
	close(gateH)

	r := wait(t, ch)
	log := out.String()
	if r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v, %v\n%s", r.res, r.err, log)
	}
	for _, id := range []string{a, b, h} {
		if got := status(t, s, id).Status; got != store.TaskVerified {
			t.Fatalf("%s = %s, want verified\n%s", id, got, log)
		}
	}
	if n := implementerAttempts(t, s, a); n != 2 {
		t.Errorf("%s has %d implementer attempts, want 2\n%s", a, n, log)
	}
	newHead := headOf(t, s, a)
	if newHead == firstHead {
		t.Fatalf("%s head did not move after its second run\n%s", a, log)
	}
	bb, err := s.GetBranch(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	// b started from a's re-verified head, not the one a had before it was reopened.
	if bb.BaseSHA != newHead {
		t.Errorf("%s base = %s, want %s's new head %s (old head %s)\n%s", b, bb.BaseSHA, a, newHead, firstHead, log)
	}
	if n := strings.Count(log, b+": started"); n != 1 {
		t.Errorf("%s started %d times, want 1\n%s", b, n, log)
	}
	if i, j := strings.LastIndex(log, a+": verified at"), strings.Index(log, b+": started"); i < 0 || j < i {
		t.Errorf("%s started before %s was verified again\n%s", b, a, log)
	}
}
