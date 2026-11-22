package keeper

import (
	"context"
	"fmt"

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
			continue
		}
		po := publish.Options{Store: k.Store, Cfg: k.Cfg, Paths: k.Paths, Host: k.Host, TaskID: t.ID}
		pr, err := publish.Publish(ctx, po)
		if _, ok := publish.ParentRefusal(err); ok {
			k.res.Queue.Waiting = append(k.res.Queue.Waiting, QueueWaiting{TaskID: t.ID, Reason: err.Error()})
			k.logf("%s: approved, waiting: %v", t.ID, err)
			continue
		}
		if err != nil {
			if err := k.block(ctx, t.ID, fmt.Sprintf("keeper: publishing the approved task failed: %v", err)); err != nil {
				return err
			}
			continue
		}
		k.res.Queue.Published = append(k.res.Queue.Published, QueuePublished{TaskID: t.ID, Ref: pr.Ref, SHA: pr.SHA})
		k.logf("%s: published from the queue as %s at %s", t.ID, pr.Ref, short(pr.SHA))
	}
	return nil
}
