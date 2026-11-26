package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/store"
)

// phased is a RunTask that moves each task through agent and gate phases on the test's
// command, using the agent slot the scheduler passes like the pipeline does. A task starts in
// an agent phase. Commands on its channel: "gate" releases the slot and enters the gate,
// "agent" waits for a slot and enters an agent phase (a fix attempt or the verifier), and a
// final status ends the run.
type phased struct {
	s      *store.Store
	cmd    map[string]chan string
	events chan string // "<id> agent" / "<id> gate" after the phase began, "<id> end" before returning

	mu          sync.Mutex
	agents      int
	maxAgents   int
	inFlight    int
	maxInFlight int
}

func newPhased(s *store.Store, ids ...string) *phased {
	p := &phased{s: s, cmd: map[string]chan string{}, events: make(chan string, 100)}
	for _, id := range ids {
		p.cmd[id] = make(chan string, 4)
	}
	return p
}

func (p *phased) count(agents, inFlight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents += agents
	p.inFlight += inFlight
	p.maxAgents = max(p.maxAgents, p.agents)
	p.maxInFlight = max(p.maxInFlight, p.inFlight)
}

func (p *phased) run(ctx context.Context, id string) (pipeline.Outcome, error) {
	bg := context.WithoutCancel(ctx)
	slot := pipeline.AgentSlotFrom(ctx)
	p.count(1, 1)
	if err := p.s.SetTaskStatus(bg, id, store.TaskRunning, store.PhaseImplement, ""); err != nil {
		return pipeline.Outcome{}, err
	}
	p.events <- id + " agent"
	inAgent := true
	for {
		var c string
		select {
		case c = <-p.cmd[id]:
		case <-ctx.Done():
			c = store.TaskBlocked
		}
		switch c {
		case "gate":
			p.count(-1, 0)
			inAgent = false
			slot.Release()
			if err := p.s.SetTaskStatus(bg, id, store.TaskRunning, store.PhaseGate, ""); err != nil {
				return pipeline.Outcome{}, err
			}
			p.events <- id + " gate"
		case "agent":
			if err := slot.Acquire(ctx); err != nil {
				return pipeline.Outcome{}, err
			}
			p.count(1, 0)
			inAgent = true
			if err := p.s.SetTaskStatus(bg, id, store.TaskRunning, store.PhaseImplement, ""); err != nil {
				return pipeline.Outcome{}, err
			}
			p.events <- id + " agent"
		default:
			if inAgent {
				p.count(-1, 0)
			}
			p.count(0, -1)
			if err := p.s.SetTaskStatus(bg, id, c, "", ""); err != nil {
				return pipeline.Outcome{}, err
			}
			p.events <- id + " end"
			return pipeline.Outcome{Status: c}, nil
		}
	}
}

// next returns the next phase event or fails after a deadline.
func (p *phased) next(t *testing.T) string {
	t.Helper()
	select {
	case ev := <-p.events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a task to enter a phase")
		return ""
	}
}

func (p *phased) expect(t *testing.T, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for range want {
		got[p.next(t)] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("phase events = %v, want %v", got, want)
		}
	}
}

func TestAgentSlotIsFreeWhileTaskIsInGate(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c"})
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	p := newPhased(s, a, b, c)
	sch := &Scheduler{Store: s, MaxParallel: 2, MaxInFlight: 3, RunTask: p.run, Sleep: pollSleep}
	ch := runAsync(context.Background(), sch, g, Options{})

	p.expect(t, a+" agent", b+" agent")
	// a enters its gate: its agent slot goes to c, a third task started while a is gated.
	p.cmd[a] <- "gate"
	p.expect(t, a+" gate", c+" agent")
	if st := status(t, s, a); st.Status != store.TaskRunning || st.Phase != store.PhaseGate {
		t.Fatalf("a = %s/%s, want running in its gate", st.Status, st.Phase)
	}

	// a returns from its gate to a fix attempt while b and c run agents: it waits for a slot
	// and runs only once b has ended.
	p.cmd[a] <- "agent"
	p.cmd[b] <- store.TaskVerified
	if got := []string{p.next(t), p.next(t)}; got[0] != b+" end" || got[1] != a+" agent" {
		t.Fatalf("phase events = %v, want b to end before a's fix attempt runs", got)
	}
	p.cmd[c] <- store.TaskVerified
	p.cmd[a] <- store.TaskVerified

	r := wait(t, ch)
	if r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v, %v", r.res, r.err)
	}
	if p.maxAgents != 2 {
		t.Fatalf("at most %d agents ran at once, want max_parallel = 2", p.maxAgents)
	}
}

func TestInFlightTasksStayWithinMaxInFlight(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"}, task{id: "b"}, task{id: "c"})
	a, b, c := store.TaskID(g, "a"), store.TaskID(g, "b"), store.TaskID(g, "c")
	p := newPhased(s, a, b, c)
	out := &syncBuf{}
	sch := &Scheduler{Store: s, MaxParallel: 1, MaxInFlight: 2, RunTask: p.run, Out: out, Sleep: pollSleep}
	ch := runAsync(context.Background(), sch, g, Options{})

	p.expect(t, a+" agent")
	p.cmd[a] <- "gate"
	p.expect(t, a+" gate", b+" agent")
	// a and b are both in their gates: the agent slot is free, but two tasks are in flight.
	p.cmd[b] <- "gate"
	p.expect(t, b+" gate")
	if st := status(t, s, c); st.Status != store.TaskPending {
		t.Fatalf("c = %s with a and b in flight, want pending", st.Status)
	}
	// a takes the free slot for its verifier and ends; only then does c start.
	p.cmd[a] <- "agent"
	p.expect(t, a+" agent")
	p.cmd[a] <- store.TaskVerified
	p.expect(t, a+" end", c+" agent")
	// b waits for c's slot to run its verifier.
	p.cmd[b] <- "agent"
	p.cmd[c] <- store.TaskVerified
	if got := []string{p.next(t), p.next(t)}; got[0] != c+" end" || got[1] != b+" agent" {
		t.Fatalf("phase events = %v, want c to end before b's verifier runs", got)
	}
	p.cmd[b] <- store.TaskVerified

	r := wait(t, ch)
	if r.err != nil || r.res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v, %v", r.res, r.err)
	}
	if p.maxInFlight != 2 || p.maxAgents != 1 {
		t.Fatalf("max in flight = %d, max agents = %d; want 2 and 1", p.maxInFlight, p.maxAgents)
	}
	if strings.Index(out.String(), c+": started") < strings.Index(out.String(), b+": started") {
		t.Fatalf("c started before b:\n%s", out.String())
	}
}

func TestMaxInFlightBelowMaxParallelIsRejected(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"})
	_, err := (&Scheduler{Store: s, MaxParallel: 3, MaxInFlight: 2, RunTask: newFake(s).run, Sleep: pollSleep}).RunGoal(context.Background(), g, Options{})
	if err == nil || !strings.Contains(err.Error(), "max_in_flight must be >= max_parallel (3), got 2") {
		t.Fatalf("err = %v, want a max_in_flight error", err)
	}
	if st := status(t, s, store.TaskID(g, "a")); st.Status != store.TaskPending {
		t.Fatalf("a = %s, want pending: nothing may start", st.Status)
	}
}

// countingPoller is a poller that counts the waits Run begins.
type countingPoller struct {
	poller
	mu    sync.Mutex
	waits int
}

func (p *countingPoller) sleep(ctx context.Context, d time.Duration) error {
	p.mu.Lock()
	p.waits++
	p.mu.Unlock()
	return p.poller.sleep(ctx, d)
}

func TestBusyTaskWithFreeAgentSlotPollsOncePerInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	g := addGoal(t, s, task{id: "a"}, task{id: "b"})
	a, b := store.TaskID(g, "a"), store.TaskID(g, "b")
	other := store.Claimer{Owner: "other", PID: 1, Command: "task run"}
	if err := s.ClaimTasks(ctx, other, []string{b}); err != nil {
		t.Fatal(err)
	}
	f := newFake(s, a, b)
	p := &countingPoller{poller: *newPoller()}
	out := &syncBuf{}
	mine := store.Claimer{Owner: "mine", PID: 2, Command: "run"}
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: p.sleep, Out: out, Claimer: mine}
	ch := runAll(ctx, sch, RunOptions{})

	if got := f.waitStart(t); got != a {
		t.Fatalf("started %s, want %s", got, a)
	}
	// a runs, a slot is free and b is busy elsewhere: Run waits a full poll interval between
	// looks at b, so each poke ends exactly one wait.
	const pokes = 5
	for range pokes {
		p.poke(t)
	}
	p.mu.Lock()
	waits := p.waits
	p.mu.Unlock()
	if waits > pokes+1 {
		t.Fatalf("Run began %d waits for %d poll intervals; a busy task must not make it poll faster", waits, pokes)
	}
	if n := strings.Count(out.String(), b+": busy"); n != 1 {
		t.Fatalf("busy logged %d times, want once:\n%s", n, out)
	}
	if err := s.ReleaseClaims(ctx, other, []string{b}); err != nil {
		t.Fatal(err)
	}
	if got := p.pokeUntilStart(t, f); got != b {
		t.Fatalf("started %s, want %s", got, b)
	}
	f.release[a] <- store.TaskVerified
	f.release[b] <- store.TaskVerified
	if r := waitAll(t, ch); resultOf(r.res, g).Status != store.GoalDone {
		t.Fatalf("Run = %+v", r.res)
	}
}
