package keeper

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/fanin"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

// restackFanIn keeps a task with several dependencies on top of them. When a parent moved or
// landed (fanin.Target.Moved), the task is restacked if a parent landed or a parent's tip
// conflicts with its head; otherwise it is left alone. The restack resets the branch to the
// deepest parent (or the remote base branch when that parent landed), merges the other parents
// that have not landed and replays the task's own commits. Conflicts go to rerere, then the
// resolver agent; any failure puts the branch back and blocks the task.
func (k *keeper) restackFanIn(ctx context.Context, t store.Task, br store.Branch) error {
	root := k.Paths.Root
	tg, err := fanin.Plan(ctx, k.Store, k.Cfg, root, t)
	if err != nil {
		return fmt.Errorf("fan-in task %s: %w", t.ID, err)
	}
	if tg.Wait != "" {
		k.skip(t.ID, tg.Wait)
		return nil
	}
	if br.HeadSHA == "" {
		k.skip(t.ID, "no recorded head commit")
		return nil
	}
	moved, err := tg.Moved(ctx, root, br)
	if err != nil {
		return fmt.Errorf("compare fan-in task %s with its dependencies: %w", t.ID, err)
	}
	if !moved {
		return nil
	}
	landed := tg.Landed || tg.Upstream != nil
	for _, p := range tg.Parents {
		landed = landed || p.Landed
	}
	if landed {
		k.logf("%s: a dependency landed; restacking onto %s (%s)", t.ID, tg.Onto, short(tg.BaseSHA))
	} else {
		// KP4/KP5: a fan-in that still merges cleanly with every parent is left alone.
		var conflicts []string
		for _, p := range tg.Parents {
			files, err := gitx.MergeConflicts(ctx, root, p.Head, br.HeadSHA)
			if err != nil {
				return fmt.Errorf("conflict check of %s against %s: %w", t.ID, p.Branch, err)
			}
			if len(files) > 0 {
				conflicts = append(conflicts, fmt.Sprintf("%s (%s)", p.Branch, strings.Join(files, ", ")))
			}
		}
		if len(conflicts) == 0 {
			return nil
		}
		k.logf("%s: conflicts with %s; restacking onto %s (%s)", t.ID, strings.Join(conflicts, "; "), tg.Onto, short(tg.BaseSHA))
	}
	return k.withClaim(ctx, t.ID, t.Status, func() error { return k.restack(ctx, t, br, tg) })
}

// restack rebuilds the fan-in task on tg with the resolver for conflicts, then settles it.
func (k *keeper) restack(ctx context.Context, t store.Task, br store.Branch, tg fanin.Target) error {
	if ok, err := k.preflight(ctx, t, br, tg.Onto); err != nil || !ok {
		return err
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	tips := []string{tg.BaseSHA}
	for _, p := range tg.Merges() {
		tips = append(tips, p.Head)
	}
	baseCommits, err := k.baseCommits(ctx, br.HeadSHA, tips...)
	if err != nil {
		return err
	}

	old := br
	fail := func(step string, files []string, detail string) error {
		if err := k.restore(ctx, old); err != nil {
			return fmt.Errorf("%s: restacking onto %s failed (%s: %s) and the branch could not be restored: %w", t.ID, tg.Onto, step, detail, err)
		}
		where := ""
		if len(files) > 0 {
			where = " in " + strings.Join(files, ", ")
		}
		return k.block(ctx, t.ID, fmt.Sprintf("keeper: restacking fan-in task onto %s (%s) failed at %s%s: %s; the branch is unchanged. Resolve the conflict by hand, then run `mill task rebase %s` or `mill run --retry-blocked`",
			tg.Onto, short(tg.BaseSHA), step, where, detail, t.ID))
	}

	// resolveErr carries the failed step of the resolver out of fanin.Restack.
	type resolveErr struct{ step, detail string }
	var failed *resolveErr
	resolver := func(ctx context.Context, stop fanin.Stop) error {
		k.logf("%s: stopped %s on conflicts in %s; running the resolver", t.ID, stop.Step, strings.Join(stop.Files, ", "))
		step, _, detail := k.resolve(ctx, t, br.Worktree, stop.Onto, baseCommits, stop.Files)
		if step != "" {
			failed = &resolveErr{step: step, detail: detail}
			return errors.New(detail)
		}
		return nil
	}
	fr, err := fanin.Restack(ctx, fanin.Options{Branch: br, Target: tg, Resolve: resolver})
	var ce *fanin.ConflictError
	switch {
	case errors.As(err, &ce) && failed != nil:
		return fail(failed.step+" ("+ce.Step+")", ce.Files, failed.detail)
	case errors.As(err, &ce):
		return fail(ce.Step, ce.Files, err.Error())
	case err != nil:
		return fail("the restack", nil, err.Error())
	}

	br.Base, br.BaseSHA, br.StartSHA, br.HeadSHA = tg.Base, tg.BaseSHA, fr.StartSHA, fr.Head
	return k.settle(ctx, t, old, br, restacked{onto: tg.Onto, tip: tg.BaseSHA, oldStart: oldStart, agent: fr.Resolved, rerere: fr.Rerere}, fail)
}
