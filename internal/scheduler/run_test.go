package scheduler

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// openStore opens an empty store whose claims count as held while their row exists.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mill.db"), store.Options{
		ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func specOf(x task) taskspec.Spec {
	owns, deps := x.owns, x.deps
	if owns == nil {
		owns = []string{x.id + ".go"}
	}
	if deps == nil {
		deps = []string{}
	}
	return taskspec.Spec{ID: x.id, Title: x.id, Kind: "implement", Spec: "do " + x.id,
		Acceptance: []string{"works"}, Checks: []string{}, Owns: owns, Size: "S", DependsOn: deps}
}

// addGoal stores an approved goal with the given tasks and returns its id.
func addGoal(t *testing.T, s *store.Store, tasks ...task) string {
	t.Helper()
	ctx := context.Background()
	p := plan.Plan{Goal: "goal", Assumptions: []string{}}
	for _, x := range tasks {
		p.Tasks = append(p.Tasks, specOf(x))
	}
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, p); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// addSingle stores a pending single task (one without a goal).
func addSingle(t *testing.T, s *store.Store, x task) {
	t.Helper()
	if err := s.CreateTask(context.Background(), specOf(x)); err != nil {
		t.Fatal(err)
	}
}

// poller is an injected Sleep that returns only when the test pokes it (or ctx ends), so a
// test decides when Run polls the store again.
type poller struct{ tick chan struct{} }

func newPoller() *poller { return &poller{tick: make(chan struct{})} }

func (p *poller) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-p.tick:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// poke ends one wait of Run. When it returns, Run has finished everything it did before that
// wait began.
func (p *poller) poke(t *testing.T) {
	t.Helper()
	select {
	case p.tick <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Run to wait for the next poll")
	}
}

// pokeUntilStart lets Run poll until a task starts and returns its id.
func (p *poller) pokeUntilStart(t *testing.T, f *fake) string {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case id := <-f.started:
			return id
		case p.tick <- struct{}{}:
		case <-timeout:
			t.Fatal("timed out waiting for a task to start")
			return ""
		}
	}
}

type runAllResult struct {
	res RunResult
	err error
}

func runAll(ctx context.Context, sch *Scheduler, o RunOptions) chan runAllResult {
	ch := make(chan runAllResult, 1)
	go func() {
		r, err := sch.Run(ctx, o)
		ch <- runAllResult{r, err}
	}()
	return ch
}

func waitAll(t *testing.T, ch chan runAllResult) runAllResult {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Run")
		return runAllResult{}
	}
}

// noStart fails when a task has started since the last waitStart.
func (f *fake) noStart(t *testing.T) {
	t.Helper()
	select {
	case id := <-f.started:
		t.Fatalf("%s started; events %v", id, f.events)
	default:
	}
}

func goalStatus(t *testing.T, s *store.Store, id string) store.Goal {
	t.Helper()
	g, err := s.GetGoal(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func resultOf(r RunResult, goalID string) Result {
	for _, g := range r.Goals {
		if g.GoalID == goalID {
			return g
		}
	}
	return Result{}
}

func TestRunStartsTasksAcrossGoalsTogether(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"}, task{id: "b", deps: []string{"a"}})
	g2 := addGoal(t, s, task{id: "c"})
	a, b, c := store.TaskID(g1, "a"), store.TaskID(g1, "b"), store.TaskID(g2, "c")
	f := newFake(s, a, c)
	p := newPoller()
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: p.sleep}
	ch := runAll(context.Background(), sch, RunOptions{})

	first := []string{f.waitStart(t), f.waitStart(t)}
	slices.Sort(first)
	if !slices.Equal(first, []string{a, c}) {
		t.Fatalf("first starts = %v, want one task of each goal (%s, %s)", first, a, c)
	}
	for _, g := range []string{g1, g2} {
		if got := goalStatus(t, s, g).Status; got != store.GoalRunning {
			t.Errorf("goal %s = %s while its task runs, want running", g, got)
		}
	}
	f.release[a] <- store.TaskVerified
	f.release[c] <- store.TaskVerified
	r := waitAll(t, ch)
	if len(r.res.Goals) != 2 || resultOf(r.res, g1).Status != store.GoalDone || resultOf(r.res, g2).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
	if f.index("start "+b) < 0 {
		t.Fatalf("b never ran: %v", f.events)
	}
}

func TestMaxParallelCapsTasksAcrossGoals(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"}, task{id: "b"})
	g2 := addGoal(t, s, task{id: "c"}, task{id: "d"})
	addSingle(t, s, task{id: "s1"})
	f := newFake(s)
	p := newPoller()
	sch := &Scheduler{Store: s, MaxParallel: 1, RunTask: f.run, Sleep: p.sleep}
	r := waitAll(t, runAll(context.Background(), sch, RunOptions{}))
	if f.maxActive != 1 || len(f.overlaps) != 0 {
		t.Fatalf("max concurrent = %d, overlaps %v; want one task at a time", f.maxActive, f.overlaps)
	}
	if len(f.events) != 10 {
		t.Fatalf("events = %v, want five tasks run", f.events)
	}
	if resultOf(r.res, g1).Status != store.GoalDone || resultOf(r.res, g2).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
	if want := []TaskResult{{TaskID: "s1", Status: store.TaskVerified}}; !slices.Equal(r.res.Tasks, want) {
		t.Fatalf("single tasks = %+v, want %+v", r.res.Tasks, want)
	}
}

func TestOwnsOverlapAcrossGoalsNeverRunTogether(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a", owns: []string{"pkg/**"}})
	g2 := addGoal(t, s, task{id: "b", owns: []string{"pkg/b.go"}}, task{id: "c", owns: []string{"other/**"}})
	a, b, c := store.TaskID(g1, "a"), store.TaskID(g2, "b"), store.TaskID(g2, "c")
	f := newFake(s, a, b, c)
	p := newPoller()
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: p.sleep}
	ch := runAll(context.Background(), sch, RunOptions{})

	first := []string{f.waitStart(t), f.waitStart(t)}
	slices.Sort(first)
	if !slices.Equal(first, []string{a, c}) {
		t.Fatalf("first starts = %v, want a and c (b overlaps a)", first)
	}
	// Two more polls while a runs: b still does not start.
	p.poke(t)
	p.poke(t)
	f.noStart(t)
	f.release[a] <- store.TaskVerified
	if got := f.waitStart(t); got != b {
		t.Fatalf("after a: started %s, want b", got)
	}
	f.release[b] <- store.TaskVerified
	f.release[c] <- store.TaskVerified
	waitAll(t, ch)
	for _, pr := range f.overlaps {
		if (pr[0] == a && pr[1] == b) || (pr[0] == b && pr[1] == a) {
			t.Fatalf("a and b ran together: %v", f.overlaps)
		}
	}
}

func TestPollStartsGoalApprovedAndSingleTaskAddedMidRun(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"})
	a := store.TaskID(g1, "a")
	f := newFake(s, a)
	p := newPoller()
	var slept []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		slept = append(slept, d)
		f.mu.Unlock()
		return p.sleep(ctx, d)
	}
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: sleep}
	ch := runAll(context.Background(), sch, RunOptions{})
	if got := f.waitStart(t); got != a {
		t.Fatalf("started %s, want %s", got, a)
	}

	// A draft goal is never run; an approved goal and a pending single task are.
	if _, err := s.CreateGoal(context.Background(), "draft", nil); err != nil {
		t.Fatal(err)
	}
	g2 := addGoal(t, s, task{id: "b"})
	addSingle(t, s, task{id: "s1"})
	p.poke(t)
	later := []string{f.waitStart(t), f.waitStart(t)}
	slices.Sort(later)
	if want := []string{store.TaskID(g2, "b"), "s1"}; !slices.Equal(later, want) {
		t.Fatalf("started mid-run = %v, want %v", later, want)
	}
	f.release[a] <- store.TaskVerified
	r := waitAll(t, ch)
	if len(r.res.Goals) != 2 || resultOf(r.res, g1).Status != store.GoalDone || resultOf(r.res, g2).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
	if want := []TaskResult{{TaskID: "s1", Status: store.TaskVerified}}; !slices.Equal(r.res.Tasks, want) {
		t.Fatalf("single tasks = %+v, want %+v", r.res.Tasks, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(slept) == 0 || slept[0] != DefaultPollInterval {
		t.Fatalf("slept %v, want the default poll interval %v", slept, DefaultPollInterval)
	}
}

func TestOneGoalBlockingAcrossGoalsDoesNotStopTheOther(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"}, task{id: "b", deps: []string{"a"}})
	g2 := addGoal(t, s, task{id: "c"}, task{id: "d", deps: []string{"c"}})
	a, b, c := store.TaskID(g1, "a"), store.TaskID(g1, "b"), store.TaskID(g2, "c")
	f := newFake(s, a, c)
	p := newPoller()
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: p.sleep}
	ch := runAll(context.Background(), sch, RunOptions{})
	f.waitStart(t)
	f.waitStart(t)
	f.release[a] <- store.TaskBlocked

	// g1 ends blocked while c of g2 still runs.
	deadline := time.Now().Add(10 * time.Second)
	for goalStatus(t, s, g1).Status != store.GoalBlocked {
		if time.Now().After(deadline) {
			t.Fatalf("goal %s did not end blocked: %+v", g1, goalStatus(t, s, g1))
		}
		p.poke(t)
	}
	if got := goalStatus(t, s, g2).Status; got != store.GoalRunning {
		t.Fatalf("goal %s = %s, want running", g2, got)
	}
	f.release[c] <- store.TaskVerified
	r := waitAll(t, ch)
	want1 := a + " blocked; waiting: " + b + " (needs " + a + ")"
	if got := resultOf(r.res, g1); got.Status != store.GoalBlocked || got.Reason != want1 {
		t.Fatalf("goal %s = %+v, want blocked %q", g1, got, want1)
	}
	if got := resultOf(r.res, g2); got.Status != store.GoalDone || got.Reason != "" {
		t.Fatalf("goal %s = %+v, want done", g2, got)
	}
	if gl := goalStatus(t, s, g1); gl.Reason != want1 {
		t.Fatalf("stored goal %s = %+v", g1, gl)
	}
}

func TestClaimedTaskStartsAfterClaimReleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	g := addGoal(t, s, task{id: "a"})
	a := store.TaskID(g, "a")
	other := store.Claimer{Owner: "other", PID: 1, Command: "task run"}
	if err := s.ClaimTasks(ctx, other, []string{a}); err != nil {
		t.Fatal(err)
	}
	f := newFake(s, a)
	p := newPoller()
	out := &syncBuf{}
	mine := store.Claimer{Owner: "mine", PID: 2, Command: "run"}
	sch := &Scheduler{Store: s, MaxParallel: 1, RunTask: f.run, Sleep: p.sleep, Out: out, Claimer: mine}
	ch := runAll(ctx, sch, RunOptions{})

	// Run keeps polling while the claim is held and does not start the task.
	p.poke(t)
	p.poke(t)
	f.noStart(t)
	if !strings.Contains(out.String(), a+": busy (claimed by mill task run (pid 1))") {
		t.Fatalf("output:\n%s", out)
	}
	if err := s.ReleaseClaims(ctx, other, []string{a}); err != nil {
		t.Fatal(err)
	}
	if got := p.pokeUntilStart(t, f); got != a {
		t.Fatalf("started %s, want %s", got, a)
	}
	claims, err := s.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claims[a].Owner != "mine" {
		t.Fatalf("claims while running = %+v, want %s claimed by the scheduler", claims, a)
	}
	f.release[a] <- store.TaskVerified
	r := waitAll(t, ch)
	if resultOf(r.res, g).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
	if claims, err := s.ClaimedTasks(ctx); err != nil || len(claims) != 0 {
		t.Fatalf("claims after the run = %+v, %v; want none", claims, err)
	}
}

func TestGoalFilterStartsOnlyThatGoal(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"})
	g2 := addGoal(t, s, task{id: "b"}, task{id: "c", deps: []string{"b"}})
	addSingle(t, s, task{id: "s1"})
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: newPoller().sleep}
	r := waitAll(t, runAll(context.Background(), sch, RunOptions{Goal: g2}))
	if want := []string{"start " + g2 + "/b", "end " + g2 + "/b", "start " + g2 + "/c", "end " + g2 + "/c"}; !slices.Equal(f.events, want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
	if len(r.res.Goals) != 1 || r.res.Goals[0] != (Result{GoalID: g2, Status: store.GoalDone}) || len(r.res.Tasks) != 0 {
		t.Fatalf("Run = %+v", r.res)
	}
	if got := goalStatus(t, s, g1).Status; got != store.GoalApproved {
		t.Fatalf("goal %s = %s, want approved (untouched)", g1, got)
	}
}

func TestTaskFilterRunsOneSingleTask(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	addGoal(t, s, task{id: "a"})
	addSingle(t, s, task{id: "s1"})
	addSingle(t, s, task{id: "s2"})
	f := newFake(s)
	f.decide = func(string) (string, string, error) { return store.TaskBlocked, "gate failed", nil }
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: newPoller().sleep}
	r := waitAll(t, runAll(context.Background(), sch, RunOptions{Task: "s2"}))
	if want := []string{"start s2", "end s2"}; !slices.Equal(f.events, want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
	if want := []TaskResult{{TaskID: "s2", Status: store.TaskBlocked, Reason: "gate failed"}}; !slices.Equal(r.res.Tasks, want) || len(r.res.Goals) != 0 {
		t.Fatalf("Run = %+v, want tasks %+v", r.res, want)
	}
}

func TestRetryBlockedAcrossGoalsRequeuesEveryGoal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"})
	g2 := addGoal(t, s, task{id: "b"})
	if err := s.SetTaskStatus(ctx, store.TaskID(g1, "a"), store.TaskBlocked, "", "gate failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, store.TaskID(g2, "b"), store.TaskFailed, "", "mill error"); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{g1, g2} {
		if err := s.SetGoalStatus(ctx, g, store.GoalBlocked, "stopped"); err != nil {
			t.Fatal(err)
		}
	}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: newPoller().sleep}
	r := waitAll(t, runAll(ctx, sch, RunOptions{Options: Options{RetryBlocked: true}}))
	if resultOf(r.res, g1).Status != store.GoalDone || resultOf(r.res, g2).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
}

func TestCancelAcrossGoalsLeavesEachGoalInterrupted(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	g1 := addGoal(t, s, task{id: "a"})
	g2 := addGoal(t, s, task{id: "b"})
	a, b := store.TaskID(g1, "a"), store.TaskID(g2, "b")
	f := newFake(s, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: newPoller().sleep}
	ch := runAll(ctx, sch, RunOptions{})
	f.waitStart(t)
	f.waitStart(t)
	cancel()
	r := waitAll(t, ch)
	for _, g := range []string{g1, g2} {
		if got := resultOf(r.res, g); got.Status != store.GoalBlocked || got.Reason != InterruptedReason(g) {
			t.Errorf("goal %s = %+v, want interrupted", g, got)
		}
	}
	for _, id := range []string{a, b} {
		if got := status(t, s, id).Status; got != store.TaskPending {
			t.Errorf("%s = %s, want pending", id, got)
		}
	}
}
