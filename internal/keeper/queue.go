package keeper

import (
	"context"
	"fmt"
	"os"

	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
)

// QueuePublished is a queued approval the pass published.
type QueuePublished struct {
	TaskID string `json:"task_id"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
}

// QueueWaiting is a queued approval that still waits for a dependency, and why.
type QueueWaiting struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Queue is what the pass did with the approval queue.
type Queue struct {
	Published []QueuePublished `json:"published"`
	Waiting   []QueueWaiting   `json:"waiting"`
}

// queue publishes the approved verified tasks, dependencies first. A refusal by the
// dependency rule leaves the approval queued; any other failure blocks the task. An approval
// of an attempt older than the task's latest one is cleared, not published.
func (k *keeper) queue(ctx context.Context) error {
	tasks, err := k.Store.ApprovedTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if g, _ := store.SplitTaskID(t.ID); k.Goal != "" && g != k.Goal {
			continue
		}
		// Reload: publishing an earlier task may have changed it.
		t, err := k.Store.GetTask(ctx, t.ID)
		if err != nil {
			return err
		}
		if !t.Approved || t.Status != store.TaskVerified {
			continue
		}
		if err := k.withClaim(ctx, t.ID, store.TaskVerified, func() error { return k.publishQueued(ctx, t.ID) }); err != nil {
			return err
		}
	}
	return nil
}

// publishQueued publishes the claimed approved task id, or clears a stale approval.
func (k *keeper) publishQueued(ctx context.Context, id string) error {
	t, err := k.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if !t.Approved {
		return nil
	}
	n, err := k.Store.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("read the latest attempt of %s: %w", t.ID, err)
	}
	if t.ApprovedAttempt < n {
		reason := fmt.Sprintf("keeper: the approval is of attempt %d but the task has attempt %d; review it again", t.ApprovedAttempt, n)
		if err := k.Store.ClearApproval(ctx, t.ID, reason); err != nil {
			return err
		}
		k.logf("%s: approval cleared: %s", t.ID, reason)
		return nil
	}
	po := k.publishOptions(t.ID)
	po.Out = nil // the pass logs the publish itself
	pr, err := publish.Publish(ctx, po)
	if _, ok := publish.ParentRefusal(err); ok {
		k.res.Queue.Waiting = append(k.res.Queue.Waiting, QueueWaiting{TaskID: t.ID, Reason: err.Error()})
		k.logf("%s: approved, waiting: %v", t.ID, err)
		return nil
	}
	if err != nil {
		return k.block(ctx, t.ID, fmt.Sprintf("keeper: publishing the approved task failed: %v", err))
	}
	k.res.Queue.Published = append(k.res.Queue.Published, QueuePublished{TaskID: t.ID, Ref: pr.Ref, SHA: pr.SHA})
	k.logf("%s: published from the queue as %s at %s", t.ID, pr.Ref, short(pr.SHA))
	publish.PrintHookReport(k.Out, os.Stderr, pr)
	return nil
}
