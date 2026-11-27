// Package scheduler runs approved goals' tasks and single tasks in dependency order, several at
// a time.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
)

// Reasons the scheduler writes.
const (
	ReasonRetry       = "retry requested"
	ReasonInterrupted = "interrupted" // set by the pipeline on a cancelled task
	ReasonLimitReset  = "usage limit reset"
)

// Waits for a usage limit to reset (--wait-on-limit).
const (
	// LimitResetSlack is added to a known reset time before resuming.
	LimitResetSlack = time.Minute
	// LimitUnknownWait is how long to wait when the reset time is unknown.
	LimitUnknownWait = 15 * time.Minute
)

// Scheduler runs the tasks of goals and single tasks.
type Scheduler struct {
	Store *store.Store
	// MaxParallel bounds the tasks running an agent at the same time. A task in its gate (or
	// waiting for a gate slot) holds no agent slot; the pipeline waits for one again before
	// its next agent phase.
	MaxParallel int
	// MaxInFlight bounds the tasks started and not yet finished; zero means MaxParallel.
	MaxInFlight int
	// Claimer claims each task before it starts; defaults to store.NewClaimer("run").
	Claimer store.Claimer
	// RunTask runs one task to its end and writes its status to the store.
	RunTask func(ctx context.Context, taskID string) (pipeline.Outcome, error)
	// Out receives progress lines; nil discards. Each line is written whole under a lock.
	Out io.Writer
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Sleep waits for d or until ctx ends, returning ctx.Err() then; defaults to a timer.
	Sleep func(ctx context.Context, d time.Duration) error

	outMu sync.Mutex
}

// Options change how a goal is run.
type Options struct {
	// RetryBlocked puts every blocked or failed task back to pending before scheduling.
	RetryBlocked bool
	// WaitOnLimit waits for a usage limit to reset and continues, instead of ending the goal
	// blocked.
	WaitOnLimit bool
	// Reprepare, when set, moves a task retried by RetryBlocked onto its current dependencies
	// (or base) before it runs, as `mill task rebase` does. It is called, with the task
	// claimed, only for a task whose branch no longer contains a dependency's branch head (or,
	// without dependencies, the tip of BaseBranch). RepoDir is the repository the branches are
	// read from.
	Reprepare  func(ctx context.Context, t store.Task) error
	RepoDir    string
	BaseBranch string
}

// Result is how the goal ended.
type Result struct {
	GoalID string `json:"goal_id"`
	Status string `json:"status"` // done or blocked
	Reason string `json:"reason,omitempty"`
}

// InterruptedReason is the goal reason after a cancelled run.
func InterruptedReason(goalID string) string {
	return fmt.Sprintf("interrupted: run `mill run %s` to continue", goalID)
}

func (s *Scheduler) logf(format string, a ...any) {
	if s.Out == nil {
		return
	}
	line := fmt.Sprintf(format+"\n", a...)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	io.WriteString(s.Out, line)
}

// agentSlots are the run's max_parallel agent slots. A task that returns from its gate and
// waits for a slot goes before a new start.
type agentSlots struct {
	mu      sync.Mutex
	max     int
	held    int
	waiting int
	free    chan struct{} // closed and replaced on every release
	wake    chan struct{} // tells the run loop a slot was released; holds at most one signal
}

func newAgentSlots(n int) *agentSlots {
	return &agentSlots{max: n, free: make(chan struct{}), wake: make(chan struct{}, 1)}
}

// tryTake takes a slot for a new task when one is free and no running task waits for one.
func (a *agentSlots) tryTake() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held >= a.max || a.waiting > 0 {
		return false
	}
	a.held++
	return true
}

// take waits for a free slot or for ctx to end.
func (a *agentSlots) take(ctx context.Context) error {
	a.mu.Lock()
	a.waiting++
	for a.held >= a.max {
		free := a.free
		a.mu.Unlock()
		select {
		case <-free:
		case <-ctx.Done():
			a.mu.Lock()
			a.waiting--
			a.mu.Unlock()
			return ctx.Err()
		}
		a.mu.Lock()
	}
	a.waiting--
	a.held++
	a.mu.Unlock()
	return nil
}

// release gives a slot back and tells the run loop it can start another task.
func (a *agentSlots) release() {
	a.untake()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// untake gives back a slot that poll took for a task it then could not start. It wakes a
// task waiting for the slot but not the run loop, which would only poll again at once.
func (a *agentSlots) untake() {
	a.mu.Lock()
	a.held--
	close(a.free)
	a.free = make(chan struct{})
	a.mu.Unlock()
}

// taskSlot is one running task's pipeline.AgentSlot.
type taskSlot struct {
	slots *agentSlots
	mu    sync.Mutex
	held  bool
}

func (t *taskSlot) Acquire(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		return nil
	}
	if err := t.slots.take(ctx); err != nil {
		return err
	}
	t.held = true
	return nil
}

func (t *taskSlot) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		t.held = false
		t.slots.release()
	}
}

type finished struct {
	id  string
	out pipeline.Outcome
	err error
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// limitHit records the tasks that hit the account's usage limit and the latest known reset.
type limitHit struct {
	ids      []string
	resetsAt time.Time // zero when no task knew it
}

// DefaultPollInterval is how often Run reads the store for new work when RunOptions does
// not set it.
const DefaultPollInterval = 2 * time.Second

// RunOptions select the work Run does.
type RunOptions struct {
	Options
	// Goal, when set, runs only this goal's tasks.
	Goal string
	// Task, when set, runs only this one task.
	Task string
	// PollInterval is how often the store is read for new work (a goal approved or a task
	// added mid-run, a claim released); defaults to DefaultPollInterval. Waited through Sleep.
	PollInterval time.Duration
}

// TaskResult is how a single task (one without a goal) ended.
type TaskResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// RunResult is how each goal Run worked on ended, in the order Run took them up, and the
// final status of each single task it ran.
type RunResult struct {
	Goals []Result     `json:"goals"`
	Tasks []TaskResult `json:"tasks"`
}

// RunGoal runs the goal's tasks until every task is verified (the goal becomes done) or
// nothing more can start (the goal becomes blocked). Cancelling ctx stops new starts, waits
// for running tasks and leaves the goal blocked so a later run continues it. A returned
// error means mill itself failed; running tasks have finished by then.
func (s *Scheduler) RunGoal(ctx context.Context, goalID string, o Options) (Result, error) {
	rr, err := s.Run(ctx, RunOptions{Options: o, Goal: goalID})
	if err != nil {
		return Result{}, err
	}
	for _, r := range rr.Goals {
		if r.GoalID == goalID {
			return r, nil
		}
	}
	return Result{}, fmt.Errorf("goal %s: the run ended without a result for it; run `mill run %s` again", goalID, goalID)
}

// runState is one Run's bookkeeping.
type runState struct {
	o       RunOptions
	claimer store.Claimer
	running map[string]store.Task
	// stuck are tasks that ran in this run and were still pending right after their run
	// ended; none starts again. A task that left pending and was reopened later runs again.
	stuck map[string]bool
	done  chan finished
	limit limitHit // the usage limit is account-wide: once hit, nothing new starts
	// goals this run works on and has not ended yet, and the order goals were taken up.
	open    map[string]bool
	order   []string
	results map[string]Result
	marked  map[string]bool // goals set running by this run since they were last ended
	singles []string        // single tasks started, in start order
	busy    map[string]bool // busy tasks already logged
	slots   *agentSlots
}

func (st *runState) take(goalID string) {
	if _, seen := st.results[goalID]; !seen && !st.open[goalID] {
		st.order = append(st.order, goalID)
	}
	st.open[goalID] = true
}

func (st *runState) selected(t store.Task) bool {
	return (st.o.Goal == "" || t.GoalID == st.o.Goal) && (st.o.Task == "" || t.ID == st.o.Task)
}

// Run runs pending tasks of every approved, running or blocked goal (and of done goals with
// an unfinished task) and pending single tasks, narrowed by o.Goal and o.Task, with
// max_parallel, owns overlap and dependencies applied across all of them. It reads the store
// again every poll interval while it runs, so work added mid-run is started. Each goal ends
// done or blocked as soon as it has nothing running and nothing more it can start. Run returns
// when nothing is running and nothing more can start, or after ctx is cancelled and running
// tasks have finished (the goals it was working on then end blocked as interrupted). A returned
// error means mill itself failed; running tasks have finished by then.
func (s *Scheduler) Run(ctx context.Context, o RunOptions) (RunResult, error) {
	if s.MaxParallel < 1 {
		return RunResult{}, fmt.Errorf("max_parallel must be >= 1, got %d", s.MaxParallel)
	}
	if s.MaxInFlight != 0 && s.MaxInFlight < s.MaxParallel {
		return RunResult{}, fmt.Errorf("max_in_flight must be >= max_parallel (%d), got %d", s.MaxParallel, s.MaxInFlight)
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	// Store writes must land even after ctx is cancelled.
	bg := context.WithoutCancel(ctx)
	st := &runState{o: o, claimer: s.Claimer, running: map[string]store.Task{}, stuck: map[string]bool{},
		done: make(chan finished), open: map[string]bool{}, results: map[string]Result{},
		marked: map[string]bool{}, busy: map[string]bool{}, slots: newAgentSlots(s.MaxParallel)}
	if st.claimer.Owner == "" {
		st.claimer = store.NewClaimer("run")
	}
	if o.Goal != "" {
		if err := s.checkGoal(bg, o.Goal); err != nil {
			return RunResult{}, err
		}
		st.take(o.Goal)
	}
	if o.Task != "" {
		if _, err := s.Store.GetTask(bg, o.Task); err != nil {
			return RunResult{}, fmt.Errorf("task %s: %w; list tasks with `mill status`", o.Task, err)
		}
	}
	if o.RetryBlocked {
		var goals []string
		switch {
		case o.Goal != "":
			goals = []string{o.Goal}
		case o.Task == "":
			sch, err := s.Store.ScheduleCandidates(bg)
			if err != nil {
				return RunResult{}, err
			}
			for _, g := range sch.Goals {
				goals = append(goals, g.ID)
			}
		}
		for _, g := range goals {
			ids, err := s.Store.ResetGoalTasks(bg, g, []string{store.TaskBlocked, store.TaskFailed}, "", ReasonRetry)
			if err != nil {
				return RunResult{}, err
			}
			for _, id := range ids {
				s.logf("%s: back to pending (%s)", id, ReasonRetry)
				if err := s.reprepare(bg, st, id); err != nil {
					return RunResult{}, err
				}
			}
			if len(ids) > 0 {
				st.take(g)
			}
		}
	}

	var loopErr error
	for {
		waiting := false
		if loopErr == nil && ctx.Err() == nil && len(st.limit.ids) == 0 {
			w, err := s.poll(ctx, bg, st)
			waiting = w
			if err != nil {
				loopErr = err
			} else if err := s.endIdleGoals(ctx, bg, st, w); err != nil {
				loopErr = err
			}
		}
		if len(st.running) == 0 {
			if len(st.limit.ids) > 0 {
				if !o.WaitOnLimit || loopErr != nil || ctx.Err() != nil {
					break
				}
				now := s.now()
				resume := now.Add(LimitUnknownWait)
				if !st.limit.resetsAt.IsZero() {
					resume = st.limit.resetsAt.Add(LimitResetSlack)
				}
				prefix := ""
				if o.Goal != "" {
					prefix = "goal " + o.Goal + ": "
				}
				s.logf("%susage limit reached; waiting until %s to resume (Ctrl-C stops)", prefix, pipeline.FormatResetTime(resume, now))
				if err := s.sleep(ctx, resume.Sub(now)); err != nil {
					break
				}
				for _, id := range st.limit.ids {
					if err := s.Store.SetTaskStatus(bg, id, store.TaskPending, "", ReasonLimitReset); err != nil {
						loopErr = err
						break
					}
					s.logf("%s: back to pending (%s)", id, ReasonLimitReset)
				}
				st.limit = limitHit{}
				continue
			}
			if loopErr != nil || ctx.Err() != nil || !waiting {
				break
			}
			// Tasks wait on a claim held by another command: look again after the interval.
			if err := s.sleep(ctx, o.PollInterval); err != nil {
				break
			}
			continue
		}
		var f finished
		if loopErr == nil && ctx.Err() == nil && len(st.limit.ids) == 0 && len(st.running) < s.maxInFlight() {
			// There is room: new work found by the next poll could start before a task ends, or
			// once a task enters its gate and frees an agent slot.
			var ok bool
			if f, ok = s.waitOrPoll(ctx, st.done, st.slots.wake, o.PollInterval); !ok {
				continue
			}
		} else {
			f = <-st.done
		}
		if err := s.finish(bg, st, f); err != nil {
			loopErr = errors.Join(loopErr, err)
		}
	}
	if loopErr != nil {
		return RunResult{}, loopErr
	}

	for _, g := range st.order {
		if st.open[g] {
			if err := s.endGoal(ctx, bg, st, g); err != nil {
				return RunResult{}, err
			}
		}
	}
	var out RunResult
	for _, g := range st.order {
		out.Goals = append(out.Goals, st.results[g])
	}
	for _, id := range st.singles {
		if ctx.Err() != nil {
			t, err := s.Store.GetTask(bg, id)
			if err != nil {
				return RunResult{}, err
			}
			// Interrupted and rate-limited single tasks continue with the next run, like goal tasks.
			if t.Status == store.TaskBlocked && (t.Reason == ReasonInterrupted || slices.Contains(st.limit.ids, id)) {
				if err := s.Store.SetTaskStatus(bg, id, store.TaskPending, "", ReasonInterrupted); err != nil {
					return RunResult{}, err
				}
			}
		}
		t, err := s.Store.GetTask(bg, id)
		if err != nil {
			return RunResult{}, err
		}
		out.Tasks = append(out.Tasks, TaskResult{TaskID: id, Status: t.Status, Reason: t.Reason})
	}
	return out, nil
}

// checkGoal refuses a goal that cannot run: a draft, a done goal with every task finished,
// or an unknown status.
func (s *Scheduler) checkGoal(ctx context.Context, goalID string) error {
	g, err := s.Store.GetGoal(ctx, goalID)
	if err != nil {
		return err
	}
	switch g.Status {
	case store.GoalApproved, store.GoalRunning, store.GoalBlocked:
	case store.GoalDraft:
		return fmt.Errorf("goal %s is a draft: review it with `mill plan show %s` and approve it with `mill plan approve %s` first", goalID, goalID, goalID)
	case store.GoalDone:
		// Safety net: a done goal with an unfinished task (sent back for changes) runs as approved.
		tasks, err := s.Store.GoalTasks(ctx, goalID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if !store.Done(t.Status) {
				return nil
			}
		}
		return fmt.Errorf("goal %s is already done: all its tasks are verified", goalID)
	default:
		return fmt.Errorf("goal %s has unknown status %q", goalID, g.Status)
	}
	return nil
}

func (s *Scheduler) maxInFlight() int {
	if s.MaxInFlight > 0 {
		return s.MaxInFlight
	}
	return s.MaxParallel
}

// waitOrPoll waits for a task to finish, an agent slot to be released or the poll interval,
// whichever comes first. ok is false when no task finished first.
func (s *Scheduler) waitOrPoll(ctx context.Context, done <-chan finished, wake <-chan struct{}, d time.Duration) (finished, bool) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tick := make(chan struct{})
	go func() {
		s.sleep(sctx, d) // an error means ctx ended; either way the loop looks again
		close(tick)
	}()
	select {
	case f := <-done:
		cancel()
		<-tick // Sleep is never called twice at once
		return f, true
	case <-wake:
		cancel()
		<-tick
		return finished{}, false
	case <-tick:
		return finished{}, false
	}
}

// finish records a task that ended and releases its claim.
func (s *Scheduler) finish(bg context.Context, st *runState, f finished) error {
	delete(st.running, f.id)
	err := s.Store.ReleaseClaims(bg, st.claimer, []string{f.id})
	if f.err != nil {
		s.logf("%s: mill error: %v", f.id, f.err)
	}
	if f.out.RateLimited {
		st.limit.ids = append(st.limit.ids, f.id)
		if f.out.ResetsAt.After(st.limit.resetsAt) {
			st.limit.resetsAt = f.out.ResetsAt
		}
	}
	t, terr := s.Store.GetTask(bg, f.id)
	// A run that reported a status left pending; if the task is pending now it was reopened
	// (e.g. by `mill review changes`) and may run again. A run that reported none and left
	// the task pending is stuck.
	if terr == nil && t.Status == store.TaskPending && (f.out.Status == "" || f.out.Status == store.TaskPending) {
		st.stuck[f.id] = true
	}
	if terr != nil {
		err = errors.Join(err, terr)
	} else if (t.Status == store.TaskVerified || t.Status == store.TaskBlocked) && f.err == nil {
		// The pipeline already logged "<task>: verified at <sha> (...)" or
		// "<task>: blocked: <reason> (task total ...)". A mill error was logged above.
	} else if t.Reason != "" {
		s.logf("%s: %s: %s", t.ID, t.Status, t.Reason)
	} else {
		s.logf("%s: %s", t.ID, t.Status)
	}
	return err
}

// poll reads the store and starts ready tasks in order while there is room: fewer than
// max_in_flight started and a free agent slot. waiting reports whether a ready task could not
// start yet: no room, its owns overlap a running task, or it is busy elsewhere.
func (s *Scheduler) poll(ctx, bg context.Context, st *runState) (waiting bool, err error) {
	sch, err := s.Store.ScheduleCandidates(bg)
	if err != nil {
		return false, err
	}
	status := map[string]string{}
	for _, t := range sch.Tasks {
		status[t.ID] = t.Status
	}
	goalStatus := map[string]string{}
	for _, g := range sch.Goals {
		goalStatus[g.ID] = g.Status
	}
	for _, t := range sch.Tasks {
		if !st.selected(t) || t.Status != store.TaskPending {
			continue
		}
		if _, ok := st.running[t.ID]; ok || st.stuck[t.ID] {
			continue
		}
		ready := true
		for _, d := range sch.Deps[t.ID] {
			ds, ok := status[d]
			if !ok {
				dt, err := s.Store.GetTask(bg, d)
				if err != nil {
					return false, err
				}
				ds = dt.Status
				status[d] = ds
			}
			if !store.Done(ds) {
				ready = false
			}
		}
		if !ready {
			continue
		}
		if len(st.running) >= s.maxInFlight() || overlapsRunning(t, st.running) || !st.slots.tryTake() {
			waiting = true
			continue
		}
		slot := &taskSlot{slots: st.slots, held: true}
		if err := s.Store.ClaimTasks(bg, st.claimer, []string{t.ID}); err != nil {
			st.slots.untake()
			var busy *store.BusyError
			if !errors.As(err, &busy) {
				return waiting, err
			}
			waiting = true
			if !st.busy[t.ID] {
				st.busy[t.ID] = true
				s.logf("%s: busy (%s); starting it when it is free", t.ID, busy.Doing)
			}
			continue
		}
		delete(st.busy, t.ID)
		// A dependency may have been reopened since the store was read: start the task only
		// if every dependency is still finished now.
		ready, err := s.depsDone(bg, sch.Deps[t.ID])
		if err != nil || !ready {
			st.slots.untake()
			if rerr := s.Store.ReleaseClaims(bg, st.claimer, []string{t.ID}); rerr != nil || err != nil {
				return waiting, errors.Join(err, rerr)
			}
			continue
		}
		if t.GoalID != "" {
			st.take(t.GoalID)
			if !st.marked[t.GoalID] {
				if goalStatus[t.GoalID] != store.GoalRunning {
					if err := s.Store.SetGoalStatus(bg, t.GoalID, store.GoalRunning, ""); err != nil {
						st.slots.untake()
						return waiting, errors.Join(err, s.Store.ReleaseClaims(bg, st.claimer, []string{t.ID}))
					}
				}
				st.marked[t.GoalID] = true
			}
		} else if !slices.Contains(st.singles, t.ID) {
			st.singles = append(st.singles, t.ID)
		}
		st.running[t.ID] = t
		s.logf("%s: started", t.ID)
		go func(id string) {
			out, err := s.RunTask(pipeline.WithAgentSlot(ctx, slot), id)
			slot.Release()
			st.done <- finished{id, out, err}
		}(t.ID)
	}
	return waiting, nil
}

// reprepare re-prepares task id, just put back to pending by --retry-blocked, when its
// starting point is stale (see stale). When re-preparing fails, or staleness cannot be
// decided, the task is blocked again with a reason saying what failed, so it never starts on
// a stale starting point. A returned error means mill itself failed.
func (s *Scheduler) reprepare(bg context.Context, st *runState, id string) error {
	if st.o.Reprepare == nil {
		return nil
	}
	block := func(what string, cause error) error {
		reason := fmt.Sprintf("%s: %v; fix it, then run `mill task rebase %s` and `mill run --retry-blocked`", what, cause, id)
		if err := s.Store.SetTaskStatus(bg, id, store.TaskBlocked, store.PhasePrepare, reason); err != nil {
			return err
		}
		s.logf("%s: %s: %s", id, store.TaskBlocked, reason)
		return nil
	}
	t, err := s.Store.GetTask(bg, id)
	if err != nil {
		return err
	}
	stale, err := s.stale(bg, st.o.Options, t)
	if err != nil {
		return block("checking whether its dependencies changed since preparation failed", err)
	}
	if !stale {
		return nil
	}
	if err := s.Store.ClaimTasks(bg, st.claimer, []string{id}); err != nil {
		var busy *store.BusyError
		if errors.As(err, &busy) {
			return block("dependencies changed since preparation but the task could not be rebased", err)
		}
		return err
	}
	rerr := st.o.Reprepare(bg, t)
	if err := s.Store.ReleaseClaims(bg, st.claimer, []string{id}); err != nil {
		return err
	}
	if rerr != nil {
		return block("dependencies changed since preparation and rebasing onto them failed", rerr)
	}
	s.logf("%s: dependencies changed since preparation; rebased", id)
	return nil
}

// stale reports whether task t's branch no longer contains its starting point: a dependency's
// current branch head, or for a task without dependencies the tip of o.BaseBranch, is not an
// ancestor of the task's branch head. A task without a branch, or with a dependency that is
// not verified or published yet, is not stale (it is retried as before); a landed dependency
// is skipped.
func (s *Scheduler) stale(bg context.Context, o Options, t store.Task) (bool, error) {
	br, err := s.Store.GetBranch(bg, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	head, err := gitx.RevParse(bg, o.RepoDir, br.Name)
	if err != nil {
		return false, fmt.Errorf("branch %q: %w", br.Name, err)
	}
	deps, err := s.Store.TaskDeps(bg, t.ID)
	if err != nil {
		return false, err
	}
	var tips []string
	if len(deps) == 0 {
		if o.BaseBranch == "" {
			return false, errors.New("no base branch is configured")
		}
		tip, err := gitx.RevParse(bg, o.RepoDir, o.BaseBranch)
		if err != nil {
			return false, fmt.Errorf("base branch %q: %w", o.BaseBranch, err)
		}
		tips = append(tips, tip)
	}
	for _, d := range deps {
		dt, err := s.Store.GetTask(bg, d)
		if err != nil {
			return false, err
		}
		if !store.Done(dt.Status) {
			return false, nil
		}
		if dt.Status == store.TaskLanded {
			continue
		}
		db, err := s.Store.GetBranch(bg, d)
		if err != nil {
			return false, fmt.Errorf("dependency %s is %s but has no branch: %w", d, dt.Status, err)
		}
		tip, err := gitx.RevParse(bg, o.RepoDir, db.Name)
		if err != nil {
			return false, fmt.Errorf("dependency branch %q: %w", db.Name, err)
		}
		tips = append(tips, tip)
	}
	for _, tip := range tips {
		ok, err := gitx.IsAncestor(bg, o.RepoDir, tip, head)
		if err != nil {
			return false, err
		}
		if !ok {
			return true, nil
		}
	}
	return false, nil
}

// depsDone reads each dependency's status from the store now and reports whether every one
// is verified, published or landed.
func (s *Scheduler) depsDone(bg context.Context, deps []string) (bool, error) {
	for _, d := range deps {
		dt, err := s.Store.GetTask(bg, d)
		if err != nil {
			return false, err
		}
		if !store.Done(dt.Status) {
			return false, nil
		}
	}
	return true, nil
}

// endIdleGoals ends each open goal that has nothing running and nothing more it can start.
// waiting is poll's report; a goal with a task waiting to start stays open.
func (s *Scheduler) endIdleGoals(ctx, bg context.Context, st *runState, waiting bool) error {
	busyGoal := map[string]bool{}
	for _, t := range st.running {
		busyGoal[t.GoalID] = true
	}
	for _, g := range st.order {
		if !st.open[g] || busyGoal[g] {
			continue
		}
		if waiting {
			// A ready task that did not start keeps its goal open.
			idle, err := s.goalIdle(bg, st, g)
			if err != nil {
				return err
			}
			if !idle {
				continue
			}
		}
		if err := s.endGoal(ctx, bg, st, g); err != nil {
			return err
		}
	}
	return nil
}

// goalIdle reports whether no selected task of the goal is ready to start.
func (s *Scheduler) goalIdle(bg context.Context, st *runState, goalID string) (bool, error) {
	tasks, err := s.Store.GoalTasks(bg, goalID)
	if err != nil {
		return false, err
	}
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	for _, t := range tasks {
		if !st.selected(t) || t.Status != store.TaskPending || st.stuck[t.ID] {
			continue
		}
		deps, err := s.Store.TaskDeps(bg, t.ID)
		if err != nil {
			return false, err
		}
		ready := true
		for _, d := range deps {
			if !store.Done(status[d]) {
				ready = false
			}
		}
		if ready {
			return false, nil
		}
	}
	return true, nil
}

// endGoal sets an open goal done or blocked with the reason RunGoal has always given.
func (s *Scheduler) endGoal(ctx, bg context.Context, st *runState, goalID string) error {
	tasks, err := s.Store.GoalTasks(bg, goalID)
	if err != nil {
		return err
	}
	res := Result{GoalID: goalID}
	allDone := true
	for _, t := range tasks {
		if !store.Done(t.Status) {
			allDone = false
		}
	}
	switch {
	case allDone:
		res.Status = store.GoalDone
	case ctx.Err() != nil:
		if _, err := s.Store.ResetGoalTasks(bg, goalID, []string{store.TaskBlocked}, ReasonInterrupted, ReasonInterrupted); err != nil {
			return err
		}
		// Rate-limited tasks continue with the next run, like interrupted ones.
		for _, t := range tasks {
			if !slices.Contains(st.limit.ids, t.ID) {
				continue
			}
			if err := s.Store.SetTaskStatus(bg, t.ID, store.TaskPending, "", ReasonInterrupted); err != nil {
				return err
			}
		}
		res.Status, res.Reason = store.GoalBlocked, InterruptedReason(goalID)
	case len(st.limit.ids) > 0:
		res.Status, res.Reason = store.GoalBlocked, s.limitReason(goalID, st.limit)
	default:
		reason, err := s.blockedReason(bg, tasks, st.stuck)
		if err != nil {
			return err
		}
		res.Status, res.Reason = store.GoalBlocked, reason
	}
	if err := s.Store.SetGoalStatus(bg, goalID, res.Status, res.Reason); err != nil {
		return err
	}
	if res.Reason != "" {
		s.logf("goal %s: %s: %s", goalID, res.Status, res.Reason)
	} else {
		s.logf("goal %s: %s", goalID, res.Status)
	}
	st.results[goalID] = res
	delete(st.open, goalID)
	delete(st.marked, goalID)
	return nil
}

// limitReason is the goal reason after a task hit the account's usage limit.
func (s *Scheduler) limitReason(goalID string, l limitHit) string {
	retry := fmt.Sprintf("`mill run %s --retry-blocked`", goalID)
	head := fmt.Sprintf("rate limited: %s hit the account's usage limit", strings.Join(l.ids, ", "))
	if l.resetsAt.IsZero() {
		return head + "; the reset time is unknown; run " + retry + " later"
	}
	return head + "; resets at " + pipeline.FormatResetTime(l.resetsAt, s.now()) + "; run " + retry + " after that"
}

func overlapsRunning(t store.Task, running map[string]store.Task) bool {
	for _, r := range running {
		for _, x := range t.Owns {
			for _, y := range r.Owns {
				if plan.GlobsOverlap(x, y) {
					return true
				}
			}
		}
	}
	return false
}

// blockedReason lists the tasks that stopped (blocked, failed or left in another state) and
// the pending tasks waiting on them. A pending task in stuck was still pending right after its
// own run in this run ended; it is reported as stuck.
func (s *Scheduler) blockedReason(ctx context.Context, tasks []store.Task, stuck map[string]bool) (string, error) {
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	var stopped, waiting []string
	for _, t := range tasks {
		switch {
		case store.Done(t.Status):
		case t.Status == store.TaskPending:
			if stuck[t.ID] {
				stopped = append(stopped, t.ID+" stuck (it did not leave pending after running)")
				continue
			}
			deps, err := s.Store.TaskDeps(ctx, t.ID)
			if err != nil {
				return "", err
			}
			var needs []string
			for _, d := range deps {
				if !store.Done(status[d]) {
					needs = append(needs, d)
				}
			}
			item := t.ID
			if len(needs) > 0 {
				item += " (needs " + strings.Join(needs, ", ") + ")"
			}
			waiting = append(waiting, item)
		default:
			item := t.ID + " " + t.Status
			if t.Reason != "" {
				item += " (" + t.Reason + ")"
			}
			stopped = append(stopped, item)
		}
	}
	reason := strings.Join(stopped, "; ")
	if len(waiting) > 0 {
		if reason != "" {
			reason += "; "
		}
		reason += "waiting: " + strings.Join(waiting, ", ")
	}
	return reason, nil
}
