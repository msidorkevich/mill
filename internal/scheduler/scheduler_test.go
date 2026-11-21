package scheduler

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mill/internal/pipeline"
	"mill/internal/plan"
	"mill/internal/store"
	"mill/internal/taskspec"
)

// task is a plan entry: id, owns, depends_on.
type task struct {
	id   string
	owns []string
	deps []string
}

// newGoal stores an approved goal with the given tasks and returns the store and goal id.
func newGoal(t *testing.T, tasks ...task) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "mill.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p := plan.Plan{Goal: "goal", Assumptions: []string{}}
	for _, x := range tasks {
		owns, deps := x.owns, x.deps
		if owns == nil {
			owns = []string{x.id + ".go"}
		}
		if deps == nil {
			deps = []string{}
		}
		p.Tasks = append(p.Tasks, taskspec.Spec{ID: x.id, Title: x.id, Kind: "implement", Spec: "do " + x.id,
			Acceptance: []string{"works"}, Checks: []string{}, Owns: owns, Size: "S", DependsOn: deps})
	}
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, p); err != nil {
		t.Fatal(err)
	}
	return s, g.ID
}

// fake is a RunTask that writes statuses through the store like the pipeline does and records
// what ran when. Tasks listed in hold wait for a status on their release channel (or for ctx
// to end, which blocks them as interrupted); others end at once with decide's status.
type fake struct {
	s       *store.Store
	decide  func(id string) (status, reason string, err error)
	hold    map[string]bool
	release map[string]chan string
	started chan string // every start, buffered

	mu        sync.Mutex
	running   []string
	maxActive int
	overlaps  [][2]string // pairs that ran at the same time
	events    []string    // "start <id>" / "end <id>"
	seen      []string    // status in the store when RunTask was called
}

func newFake(s *store.Store, hold ...string) *fake {
	f := &fake{s: s, hold: map[string]bool{}, release: map[string]chan string{}, started: make(chan string, 100)}
	for _, id := range hold {
		f.hold[id] = true
		f.release[id] = make(chan string, 1)
	}
	return f
}

func (f *fake) run(ctx context.Context, id string) (pipeline.Outcome, error) {
	bg := context.WithoutCancel(ctx)
	t, err := f.s.GetTask(bg, id)
	if err != nil {
		return pipeline.Outcome{}, err
	}
	f.mu.Lock()
	f.seen = append(f.seen, id+"="+t.Status)
	for _, r := range f.running {
		f.overlaps = append(f.overlaps, [2]string{r, id})
	}
	f.running = append(f.running, id)
	f.maxActive = max(f.maxActive, len(f.running))
	f.events = append(f.events, "start "+id)
	f.mu.Unlock()
	if err := f.s.SetTaskStatus(bg, id, store.TaskRunning, store.PhaseImplement, ""); err != nil {
		return pipeline.Outcome{}, err
	}
	f.started <- id

	status, reason := store.TaskVerified, ""
	var runErr error
	if f.hold[id] {
		select {
		case status = <-f.release[id]:
		case <-ctx.Done():
			status, reason = store.TaskBlocked, "interrupted"
		}
	} else if f.decide != nil {
		status, reason, runErr = f.decide(id)
	}

	f.mu.Lock()
	f.running = slices.DeleteFunc(f.running, func(r string) bool { return r == id })
	f.events = append(f.events, "end "+id)
	f.mu.Unlock()
	if err := f.s.SetTaskStatus(bg, id, status, "", reason); err != nil {
		return pipeline.Outcome{}, err
	}
	return pipeline.Outcome{Status: status, Reason: reason}, runErr
}

// waitStart returns the next started task id or fails after a deadline.
func (f *fake) waitStart(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.started:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a task to start")
		return ""
	}
}

func (f *fake) index(ev string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Index(f.events, ev)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type runResult struct {
	res Result
	err error
}

func runAsync(ctx context.Context, sch *Scheduler, goalID string, o Options) chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		r, err := sch.RunGoal(ctx, goalID, o)
		ch <- runResult{r, err}
	}()
	return ch
}

func wait(t *testing.T, ch chan runResult) runResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for RunGoal")
		return runResult{}
	}
}

func status(t *testing.T, s *store.Store, id string) store.Task {
	t.Helper()
	tk, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestDependenciesRunBeforeDependents(t *testing.T) {
	s, g := newGoal(t, task{id: "c", deps: []string{"a", "b"}}, task{id: "b", deps: []string{"a"}}, task{id: "a"})
	f := newFake(s)
	out := &syncBuf{}
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Out: out}
	res, err := sch.RunGoal(context.Background(), g, Options{})
	if err != nil || res.Status != store.GoalDone || res.Reason != "" {
		t.Fatalf("RunGoal = %+v, %v", res, err)
	}
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	if f.index("end "+a) > f.index("start "+b) || f.index("end "+b) > f.index("start "+c) || f.index("start "+c) < 0 {
		t.Fatalf("order: %v", f.events)
	}
	if gl, _ := s.GetGoal(context.Background(), g); gl.Status != store.GoalDone {
		t.Fatalf("goal = %+v", gl)
	}
	for _, want := range []string{a + ": started", a + ": verified", c + ": verified", "goal " + g + ": done"} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunsIndependentTasksConcurrentlyUpToMaxParallel(t *testing.T) {
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c"}, task{id: "d"})
	ids := []string{store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c"), store.TaskID(g, "d")}
	f := newFake(s, ids...)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run}
	ch := runAsync(context.Background(), sch, g, Options{})

	// Two tasks are running at the same time before either is released.
	first := []string{f.waitStart(t), f.waitStart(t)}
	slices.Sort(first)
	if !slices.Equal(first, ids[:2]) {
		t.Fatalf("first starts = %v, want plan order %v", first, ids[:2])
	}
	for _, id := range ids {
		if id != ids[0] && id != ids[1] {
			f.waitStart(t) // the next one starts only after a release below
		}
		f.release[id] <- store.TaskVerified
	}
	r := wait(t, ch)
	if r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v", r)
	}
	if f.maxActive != 2 {
		t.Fatalf("max concurrent = %d, want 2", f.maxActive)
	}
}

func TestOverlappingOwnsNeverRunTogether(t *testing.T) {
	s, g := newGoal(t,
		task{id: "a", owns: []string{"pkg/**"}},
		task{id: "b", owns: []string{"docs/x.md", "pkg/b.go"}},
		task{id: "c", owns: []string{"other/**"}})
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	f := newFake(s, a, b, c)
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run}
	ch := runAsync(context.Background(), sch, g, Options{})

	first := []string{f.waitStart(t), f.waitStart(t)}
	slices.Sort(first)
	if !slices.Equal(first, []string{a, c}) {
		t.Fatalf("first starts = %v, want a and c (b overlaps a)", first)
	}
	f.release[a] <- store.TaskVerified
	if got := f.waitStart(t); got != b {
		t.Fatalf("after a: started %s, want b", got)
	}
	f.release[b] <- store.TaskVerified
	f.release[c] <- store.TaskVerified
	if r := wait(t, ch); r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v", r)
	}
	for _, p := range f.overlaps {
		if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
			t.Fatalf("a and b ran together: %v", f.overlaps)
		}
	}
	if len(f.overlaps) < 2 {
		t.Fatalf("independent tasks did not overlap: %v", f.overlaps)
	}
}

func TestBlockedDependencyLeavesDependentsPendingAndGoalBlocked(t *testing.T) {
	s, g := newGoal(t, task{id: "a"}, task{id: "b", deps: []string{"a"}}, task{id: "c", deps: []string{"b"}},
		task{id: "d"}, task{id: "e"})
	a, b, c, d, e := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c"), store.TaskID(g, "d"), store.TaskID(g, "e")
	f := newFake(s)
	f.decide = func(id string) (string, string, error) {
		switch id {
		case a:
			return store.TaskBlocked, "gate failed after 3 attempts", nil
		case e:
			// The pipeline marks the task failed before returning its error.
			return store.TaskFailed, "mill error: disk full", errors.New("disk full")
		}
		return store.TaskVerified, "", nil
	}
	out := &syncBuf{}
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Out: out}
	res, err := sch.RunGoal(context.Background(), g, Options{})
	if err != nil || res.Status != store.GoalBlocked {
		t.Fatalf("RunGoal = %+v, %v", res, err)
	}
	for _, want := range []string{a + " blocked (gate failed after 3 attempts)", e + " failed (mill error: disk full)",
		"waiting: " + b + " (needs " + a + "), " + c + " (needs " + b + ")"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q lacks %q", res.Reason, want)
		}
	}
	if strings.Contains(res.Reason, d) {
		t.Errorf("reason names verified task d: %q", res.Reason)
	}
	for id, want := range map[string]string{a: store.TaskBlocked, b: store.TaskPending, c: store.TaskPending, d: store.TaskVerified, e: store.TaskFailed} {
		if got := status(t, s, id).Status; got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	gl, _ := s.GetGoal(context.Background(), g)
	if gl.Status != store.GoalBlocked || gl.Reason != res.Reason {
		t.Fatalf("goal = %+v", gl)
	}
	for _, want := range []string{e + ": mill error: disk full", a + ": blocked: gate failed", "goal " + g + ": blocked: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDraftAndDoneGoalsAreRefused(t *testing.T) {
	ctx := context.Background()
	s, g := newGoal(t, task{id: "a"})
	draft, err := s.CreateGoal(ctx, "draft", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 1, RunTask: f.run}
	if _, err := sch.RunGoal(ctx, draft.ID, Options{}); err == nil || !strings.Contains(err.Error(), "draft") ||
		!strings.Contains(err.Error(), "mill plan approve "+draft.ID) {
		t.Fatalf("draft: %v", err)
	}
	if err := s.SetGoalStatus(ctx, g, store.GoalDone, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sch.RunGoal(ctx, g, Options{}); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("done: %v", err)
	}
	if len(f.events) != 0 {
		t.Fatalf("tasks ran: %v", f.events)
	}
	if gl, _ := s.GetGoal(ctx, draft.ID); gl.Status != store.GoalDraft {
		t.Fatalf("draft goal changed: %+v", gl)
	}
}

func TestCancelStopsStartsWaitsAndRequeuesInterruptedTasks(t *testing.T) {
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c"}, task{id: "d", deps: []string{"a"}})
	a, b, c, d := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c"), store.TaskID(g, "d")
	f := newFake(s, a, b, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run}
	ch := runAsync(ctx, sch, g, Options{})
	f.waitStart(t)
	f.waitStart(t)
	cancel()
	r := wait(t, ch)
	if r.err != nil || r.res.Status != store.GoalBlocked || r.res.Reason != "interrupted: run `mill run "+g+"` to continue" {
		t.Fatalf("RunGoal = %+v", r)
	}
	if slices.Contains(f.events, "start "+c) || slices.Contains(f.events, "start "+d) {
		t.Fatalf("started after cancel: %v", f.events)
	}
	// Both running tasks returned before RunGoal did.
	if !slices.Contains(f.events, "end "+a) || !slices.Contains(f.events, "end "+b) {
		t.Fatalf("did not wait for running tasks: %v", f.events)
	}
	for _, id := range []string{a, b, c, d} {
		if got := status(t, s, id); got.Status != store.TaskPending {
			t.Errorf("%s = %+v, want pending", id, got)
		}
	}
	gl, _ := s.GetGoal(context.Background(), g)
	if gl.Status != store.GoalBlocked || gl.Reason != r.res.Reason {
		t.Fatalf("goal = %+v", gl)
	}
}

func TestRetryBlockedRequeuesBlockedAndFailedTasks(t *testing.T) {
	ctx := context.Background()
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c", deps: []string{"a", "b"}})
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	if err := s.SetTaskStatus(ctx, a, store.TaskBlocked, store.PhaseGate, "gate failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, b, store.TaskFailed, "", "mill error: x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGoalStatus(ctx, g, store.GoalBlocked, "a blocked"); err != nil {
		t.Fatal(err)
	}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run}

	// Without the option nothing can start and the goal stays blocked.
	res, err := sch.RunGoal(ctx, g, Options{})
	if err != nil || res.Status != store.GoalBlocked || len(f.events) != 0 {
		t.Fatalf("plain run = %+v, %v, events %v", res, err, f.events)
	}

	res, err = sch.RunGoal(ctx, g, Options{RetryBlocked: true})
	if err != nil || res.Status != store.GoalDone {
		t.Fatalf("retry run = %+v, %v", res, err)
	}
	for _, want := range []string{a + "=pending", b + "=pending", c + "=pending"} {
		if !slices.Contains(f.seen, want) {
			t.Errorf("RunTask did not see %s: %v", want, f.seen)
		}
	}
	evs, err := s.TaskEvents(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		found = found || strings.Contains(string(e.Data), "retry requested")
	}
	if !found {
		t.Fatalf("no retry requested event for a: %+v", evs)
	}
}

func TestTaskLeftPendingIsStartedOnceAndReportedStuck(t *testing.T) {
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c", deps: []string{"a"}})
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	f := newFake(s)
	var mu sync.Mutex
	calls := map[string]int{}
	run := func(ctx context.Context, id string) (pipeline.Outcome, error) {
		if id == a {
			mu.Lock()
			calls[id]++
			mu.Unlock()
			return pipeline.Outcome{}, nil // returns without leaving pending
		}
		return f.run(ctx, id)
	}
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: run}
	ch := runAsync(context.Background(), sch, g, Options{})
	r := wait(t, ch)
	if r.err != nil || r.res.Status != store.GoalBlocked {
		t.Fatalf("RunGoal = %+v, %v", r.res, r.err)
	}
	mu.Lock()
	n := calls[a]
	mu.Unlock()
	if n != 1 {
		t.Errorf("RunTask called %d times for %s, want 1", n, a)
	}
	if !strings.Contains(r.res.Reason, a+" stuck") {
		t.Errorf("reason %q does not name %s as stuck", r.res.Reason, a)
	}
	if got := status(t, s, b).Status; got != store.TaskVerified {
		t.Errorf("%s = %s, want verified", b, got)
	}
	if got := status(t, s, c).Status; got != store.TaskPending {
		t.Errorf("%s = %s, want pending", c, got)
	}
}
