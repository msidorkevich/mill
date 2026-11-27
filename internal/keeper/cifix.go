package keeper

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/store"
)

// CIFixStartedEvent records that the keeper started the fixer for a CI failure; its seq is the
// ci-status event acted on, so each failure record is fixed once.
const CIFixStartedEvent = "ci-fix-started"

// fixCI runs the fixer (KP7) for every published task whose latest CI status is a failure of
// its current published head that no earlier pass acted on. It does nothing unless ci_fix is
// set. A failure of an older head is ignored: the next poll queries the current one.
func (k *keeper) fixCI(ctx context.Context) error {
	if !k.Cfg.CIFix {
		return nil
	}
	tasks, err := k.Store.TaskSummaries(ctx, store.TaskPublished)
	if err != nil {
		return fmt.Errorf("list the published tasks to fix their CI failures: %w", err)
	}
	for _, ts := range tasks {
		if g, _ := store.SplitTaskID(ts.ID); k.Goal != "" && g != k.Goal {
			continue
		}
		if len(k.Tasks) > 0 && !slices.Contains(k.Tasks, ts.ID) {
			continue
		}
		_, due, err := k.ciFailure(ctx, ts.ID)
		if err != nil {
			return err
		}
		if !due {
			continue
		}
		if err := k.withClaim(ctx, ts.ID, store.TaskPublished, func() error {
			// Read again under the claim: another process may have acted on it meanwhile.
			rec, due, err := k.ciFailure(ctx, ts.ID)
			if err != nil || !due {
				return err
			}
			return k.ciFix(ctx, ts.ID, rec)
		}); err != nil {
			return err
		}
	}
	return nil
}

// ciFailure returns the latest ci-status record of task id and whether the fixer is due for
// it: it is a failure of the task's current published head and no ci-fix-started event names it.
func (k *keeper) ciFailure(ctx context.Context, id string) (cistatus.Record, bool, error) {
	br, err := k.Store.GetBranch(ctx, id)
	if err != nil {
		return cistatus.Record{}, false, fmt.Errorf("task %s is published but its branch cannot be loaded: %w", id, err)
	}
	rec, found, err := cistatus.Latest(ctx, k.Store, id)
	if err != nil {
		return cistatus.Record{}, false, err
	}
	if !found || rec.State != "failure" || br.PublishedSHA == "" || rec.SHA != br.PublishedSHA {
		return rec, false, nil
	}
	evs, err := k.Store.TaskEvents(ctx, id)
	if err != nil {
		return rec, false, fmt.Errorf("read the events of %s to see whether its CI failure was acted on: %w", id, err)
	}
	for _, e := range evs {
		if e.Kind != CIFixStartedEvent {
			continue
		}
		var d struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return rec, false, fmt.Errorf("corrupt %s event %d of %s: %w; check the mill database", CIFixStartedEvent, e.Seq, id, err)
		}
		if d.Seq == rec.Seq {
			return rec, false, nil
		}
	}
	return rec, true, nil
}

// ciFix runs the fixer on the claimed published task id for the CI failure rec: the failing
// checks become the findings of an implement attempt; once verified (the pipeline's gate at
// the new head and its verifier passed) the task is re-published, so the next poll queries CI
// at the new head. A task the fixer does not get verified is blocked.
func (k *keeper) ciFix(ctx context.Context, id string, rec cistatus.Record) error {
	failing := cistatus.FailingChecks(rec)
	names := make([]string, len(failing))
	checks := make([]pipeline.CICheck, len(failing))
	for i, c := range failing {
		names[i] = c.Name
		checks[i] = pipeline.CICheck(c)
	}
	what := fmt.Sprintf("keeper: CI failed at %s: %s", short(rec.SHA), strings.Join(names, ", "))
	goalID, _ := store.SplitTaskID(id)
	if _, err := k.Store.AppendEvent(ctx, CIFixStartedEvent, goalID, id, map[string]any{"seq": rec.Seq, "sha": rec.SHA}); err != nil {
		return fmt.Errorf("record that the fixer starts on the CI failure of %s: %w", id, err)
	}
	if err := k.Pipeline.RecordCIFailure(ctx, id, checks); err != nil {
		return k.block(ctx, id, fmt.Sprintf("%s; the failure could not be recorded for the fixer: %v", what, err))
	}
	if err := k.activity(ctx, id, "fix"); err != nil {
		return err
	}
	k.logf("%s: CI failed at %s (%s); running the fixer", id, short(rec.SHA), strings.Join(names, ", "))
	out, err := k.Pipeline.Run(ctx, id)
	if err != nil {
		return fmt.Errorf("fixer for %s: %w", id, err)
	}
	if out.Status != store.TaskVerified {
		return k.block(ctx, id, fmt.Sprintf("%s; the fixer did not get it verified: %s", what, out.Reason))
	}
	k.res.Fixed = append(k.res.Fixed, id)
	k.logf("%s: fixed at %s", id, short(out.HeadSHA))
	// The review branch still holds the old head: the task stays published, so that the
	// re-publish is an update (on_update runs) rather than a first publish.
	if err := k.Store.SetTaskStatus(ctx, id, store.TaskPublished, "", ""); err != nil {
		return fmt.Errorf("record %s as published again after the CI fix: %w", id, err)
	}
	return k.republish(ctx, id)
}
