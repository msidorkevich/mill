package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/fanin"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

func runTaskRebase(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "task rebase")
	id, code, ok := goalArg(env, "task rebase", "mill task rebase <id>", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task rebase: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	release, err := claimTasks(ctx, env, s, "task rebase", id)
	if err != nil {
		return fail(err)
	}
	defer release()

	t, err := s.GetTask(ctx, id)
	if err != nil {
		return fail(err)
	}
	switch t.Status {
	case store.TaskPending, store.TaskBlocked, store.TaskFailed:
	default:
		return fail(fmt.Errorf("task %s is %s: only a pending, blocked or failed task can be rebased", id, t.Status))
	}
	br, err := s.GetBranch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(fmt.Errorf("task %s has no branch yet, so there is nothing to rebase; run it first", id))
	}
	if err != nil {
		return fail(err)
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	if oldStart == "" {
		return fail(fmt.Errorf("task %s has no recorded starting point; cannot tell which commits are its own", id))
	}

	deps, err := s.TaskDeps(ctx, id)
	if err != nil {
		return fail(err)
	}
	var newBase, newTip string
	switch len(deps) {
	case 0:
		newBase = cfg.BaseBranch
		if newTip, err = gitx.RevParse(ctx, paths.Root, newBase); err != nil {
			return fail(fmt.Errorf("base branch %q: %w", newBase, err))
		}
	case 1:
		dt, err := s.GetTask(ctx, deps[0])
		if err != nil {
			return fail(err)
		}
		if !store.Done(dt.Status) {
			return fail(fmt.Errorf("dependency %s is %s, not verified, published or landed; run it first", deps[0], dt.Status))
		}
		db, err := s.GetBranch(ctx, deps[0])
		if err != nil {
			return fail(fmt.Errorf("dependency %s is %s but has no branch: %w", deps[0], dt.Status, err))
		}
		newBase = db.Name
		if newTip, err = gitx.RevParse(ctx, paths.Root, db.Name); err != nil {
			return fail(fmt.Errorf("dependency branch %q: %w", db.Name, err))
		}
	default:
		return rebaseFanIn(ctx, env, s, cfg, paths.Root, t, br, oldStart)
	}

	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return fail(fmt.Errorf("worktree %s is not a checkout of %s; restore it before rebasing", br.Worktree, br.Name))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return fail(err)
	}
	if dirty {
		return fail(fmt.Errorf("worktree %s has uncommitted changes; commit or discard them, then rebase again", br.Worktree))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return fail(err)
	}
	upToDate, err := gitx.IsAncestor(ctx, br.Worktree, newTip, head)
	if err != nil {
		return fail(err)
	}
	if upToDate {
		fmt.Fprintf(env.Stdout, "%s is already up to date: %s already contains %s (%s). Nothing changed.\n", id, br.Name, newBase, short(newTip))
		return ExitOK
	}

	newHead, err := gitx.RebaseOnto(ctx, br.Worktree, newTip, oldStart)
	var ce *gitx.RebaseConflictError
	if errors.As(err, &ce) {
		fmt.Fprintf(env.Stderr, "mill task rebase: rebasing %s onto %s (%s) conflicts; the rebase was aborted and the branch is unchanged.\nConflicting files:\n", id, newBase, short(newTip))
		for _, f := range ce.Files {
			fmt.Fprintf(env.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(env.Stderr, "Resolve by hand in %s, or edit the task and run it again from scratch.\n", br.Worktree)
		return ExitError
	}
	if err != nil {
		return fail(err)
	}

	br.BaseSHA, br.StartSHA, br.HeadSHA = newTip, newTip, newHead
	if err := s.SaveBranch(ctx, br); err != nil {
		return fail(fmt.Errorf("the branch was rebased to %s but recording it failed: %w; fix the problem and run `mill task rebase %s` again", short(newHead), err, id))
	}
	goalID, _ := store.SplitTaskID(id)
	if _, err := s.AppendEvent(ctx, "task-rebased", goalID, id, map[string]any{"old_start": oldStart, "new_start": newTip}); err != nil {
		return fail(err)
	}

	fmt.Fprintf(env.Stdout, "Rebased %s onto %s.\n  starting point  %s -> %s\n  head            %s -> %s\n", id, newBase, short(oldStart), short(newTip), short(head), short(newHead))
	if goalID != "" {
		fmt.Fprintf(env.Stdout, "Next: mill run %s --retry-blocked\n", goalID)
	} else {
		fmt.Fprintln(env.Stdout, "Next: mill task run --file <file> --retry")
	}
	return ExitOK
}

// rebaseFanIn restacks a task with several dependencies: it resets the branch to its deepest
// dependency (or the remote base branch when that one landed), merges the other dependencies
// that have not landed and replays the task's own commits. A conflict aborts and leaves the
// branch unchanged.
func rebaseFanIn(ctx context.Context, env Env, s *store.Store, cfg config.Config, root string, t store.Task, br store.Branch, oldStart string) int {
	id := t.ID
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task rebase: %v\n", err)
		return ExitError
	}
	tg, err := fanin.Plan(ctx, s, cfg, root, t)
	if err != nil {
		return fail(err)
	}
	if tg.Wait != "" {
		return fail(fmt.Errorf("task %s cannot be rebased yet: %s; run it first", id, tg.Wait))
	}
	// Like a task with one dependency, use the dependencies' branches as they are in git.
	for i, p := range tg.Parents {
		if p.Landed {
			continue
		}
		tip, err := gitx.RevParse(ctx, root, p.Branch)
		if err != nil {
			return fail(fmt.Errorf("dependency branch %q: %w", p.Branch, err))
		}
		tg.Parents[i].Head = tip
		if i == 0 {
			tg.BaseSHA = tip
		}
	}

	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return fail(fmt.Errorf("worktree %s is not a checkout of %s; restore it before rebasing", br.Worktree, br.Name))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return fail(err)
	}
	if dirty {
		return fail(fmt.Errorf("worktree %s has uncommitted changes; commit or discard them, then rebase again", br.Worktree))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return fail(err)
	}
	if head != br.HeadSHA {
		return fail(fmt.Errorf("branch %s is at %s but mill recorded head %s; reset it to the recorded head before rebasing", br.Name, short(head), short(br.HeadSHA)))
	}
	moved, err := tg.Moved(ctx, root, br)
	if err != nil {
		return fail(err)
	}
	if !moved {
		fmt.Fprintf(env.Stdout, "%s is already up to date: %s already contains its dependencies. Nothing changed.\n", id, br.Name)
		return ExitOK
	}

	fr, err := fanin.Restack(ctx, fanin.Options{Branch: br, Target: tg})
	var ce *fanin.ConflictError
	if errors.As(err, &ce) {
		fmt.Fprintf(env.Stderr, "mill task rebase: restacking %s onto %s (%s) conflicts at %s; the restack was aborted and the branch is unchanged.\nConflicting files:\n", id, tg.Onto, short(tg.BaseSHA), ce.Step)
		for _, f := range ce.Files {
			fmt.Fprintf(env.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(env.Stderr, "Resolve by hand in %s, or edit the task and run it again from scratch.\n", br.Worktree)
		return ExitError
	}
	if err != nil {
		return fail(err)
	}

	br.Base, br.BaseSHA, br.StartSHA, br.HeadSHA = tg.Base, tg.BaseSHA, fr.StartSHA, fr.Head
	if err := s.SaveBranch(ctx, br); err != nil {
		return fail(fmt.Errorf("the branch was restacked to %s but recording it failed: %w; reset %s to %s by hand, or record the new head", short(fr.Head), err, br.Name, short(head)))
	}
	goalID, _ := store.SplitTaskID(id)
	if _, err := s.AppendEvent(ctx, "task-rebased", goalID, id, map[string]any{"old_start": oldStart, "new_start": fr.StartSHA, "onto": tg.Onto, "merged": fr.Merged}); err != nil {
		return fail(err)
	}

	merged := "none"
	if len(fr.Merged) > 0 {
		merged = strings.Join(fr.Merged, ", ")
	}
	fmt.Fprintf(env.Stdout, "Restacked %s onto %s and merged %s.\n  starting point  %s -> %s\n  head            %s -> %s\n",
		id, tg.Onto, merged, short(oldStart), short(fr.StartSHA), short(head), short(fr.Head))
	if goalID != "" {
		fmt.Fprintf(env.Stdout, "Next: mill run %s --retry-blocked\n", goalID)
	}
	return ExitOK
}
