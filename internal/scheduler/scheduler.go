// Package scheduler runs an approved goal's tasks in dependency order, several at a time.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"mill/internal/pipeline"
	"mill/internal/plan"
	"mill/internal/store"
)

// Reasons the scheduler writes.
const (
	ReasonRetry       = "retry requested"
	ReasonInterrupted = "interrupted" // set by the pipeline on a cancelled task
)

// Scheduler runs the tasks of one goal.
type Scheduler struct {
	Store       *store.Store
	MaxParallel int
	// RunTask runs one task to its end and writes its status to the store.
	RunTask func(ctx context.Context, taskID string) (pipeline.Outcome, error)
	// Out receives progress lines; nil discards. Each line is written whole under a lock.
	Out io.Writer

	outMu sync.Mutex
}

// Options change how a goal is run.
type Options struct {
	// RetryBlocked puts every blocked or failed task back to pending before scheduling.
	RetryBlocked bool
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

type finished struct {
	id  string
	err error
}

// RunGoal runs the goal's tasks until every task is verified (the goal becomes done) or
// nothing more can start (the goal becomes blocked). Cancelling ctx stops new starts, waits
// for running tasks and leaves the goal blocked so a later run continues it. A returned
// error means mill itself failed; running tasks have finished by then.
func (s *Scheduler) RunGoal(ctx context.Context, goalID string, o Options) (Result, error) {
	if s.MaxParallel < 1 {
		return Result{}, fmt.Errorf("max_parallel must be >= 1, got %d", s.MaxParallel)
	}
	// Store writes must land even after ctx is cancelled.
	bg := context.WithoutCancel(ctx)
	g, err := s.Store.GetGoal(bg, goalID)
	if err != nil {
		return Result{}, err
	}
	switch g.Status {
	case store.GoalApproved, store.GoalRunning, store.GoalBlocked:
	case store.GoalDraft:
		return Result{}, fmt.Errorf("goal %s is a draft: review it with `mill plan show %s` and approve it with `mill plan approve %s` first", goalID, goalID, goalID)
	case store.GoalDone:
		return Result{}, fmt.Errorf("goal %s is already done: all its tasks are verified", goalID)
	default:
		return Result{}, fmt.Errorf("goal %s has unknown status %q", goalID, g.Status)
	}

	if o.RetryBlocked {
		ids, err := s.Store.ResetGoalTasks(bg, goalID, []string{store.TaskBlocked, store.TaskFailed}, "", ReasonRetry)
		if err != nil {
			return Result{}, err
		}
		for _, id := range ids {
			s.logf("%s: back to pending (%s)", id, ReasonRetry)
		}
	}
	if err := s.Store.SetGoalStatus(bg, goalID, store.GoalRunning, ""); err != nil {
		return Result{}, err
	}

	running := map[string]store.Task{}
	started := map[string]bool{} // tasks started in this call; none starts twice
	done := make(chan finished)
	var loopErr error
	for {
		if loopErr == nil && ctx.Err() == nil {
			if err := s.startReady(ctx, bg, goalID, running, started, done); err != nil {
				loopErr = err
			}
		}
		if len(running) == 0 {
			break
		}
		f := <-done
		delete(running, f.id)
		if f.err != nil {
			s.logf("%s: mill error: %v", f.id, f.err)
		}
		if t, err := s.Store.GetTask(bg, f.id); err != nil {
			loopErr = errors.Join(loopErr, err)
		} else if t.Reason != "" {
			s.logf("%s: %s: %s", t.ID, t.Status, t.Reason)
		} else {
			s.logf("%s: %s", t.ID, t.Status)
		}
	}
	if loopErr != nil {
		return Result{}, loopErr
	}

	tasks, err := s.Store.GoalTasks(bg, goalID)
	if err != nil {
		return Result{}, err
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
			return Result{}, err
		}
		res.Status, res.Reason = store.GoalBlocked, InterruptedReason(goalID)
	default:
		reason, err := s.blockedReason(bg, tasks, started)
		if err != nil {
			return Result{}, err
		}
		res.Status, res.Reason = store.GoalBlocked, reason
	}
	if err := s.Store.SetGoalStatus(bg, goalID, res.Status, res.Reason); err != nil {
		return Result{}, err
	}
	if res.Reason != "" {
		s.logf("goal %s: %s: %s", goalID, res.Status, res.Reason)
	} else {
		s.logf("goal %s: %s", goalID, res.Status)
	}
	return res, nil
}

// startReady starts ready tasks in plan order while there is room.
func (s *Scheduler) startReady(ctx, bg context.Context, goalID string, running map[string]store.Task, started map[string]bool, done chan<- finished) error {
	tasks, err := s.Store.GoalTasks(bg, goalID)
	if err != nil {
		return err
	}
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	for _, t := range tasks {
		if len(running) >= s.MaxParallel {
			return nil
		}
		if t.Status != store.TaskPending {
			continue
		}
		if _, ok := running[t.ID]; ok || started[t.ID] {
			continue
		}
		deps, err := s.Store.TaskDeps(bg, t.ID)
		if err != nil {
			return err
		}
		ready := true
		for _, d := range deps {
			if !store.Done(status[d]) {
				ready = false
			}
		}
		if !ready || overlapsRunning(t, running) {
			continue
		}
		running[t.ID] = t
		started[t.ID] = true
		s.logf("%s: started", t.ID)
		go func(id string) {
			_, err := s.RunTask(ctx, id)
			done <- finished{id, err}
		}(t.ID)
	}
	return nil
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
// the pending tasks waiting on them. A pending task in started ran but did not leave pending;
// it is reported as stuck.
func (s *Scheduler) blockedReason(ctx context.Context, tasks []store.Task, started map[string]bool) (string, error) {
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	var stopped, waiting []string
	for _, t := range tasks {
		switch {
		case store.Done(t.Status):
		case t.Status == store.TaskPending:
			if started[t.ID] {
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
