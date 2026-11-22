package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mill/internal/gitx"
	"mill/internal/runlock"
	"mill/internal/store"
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

	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "mill task rebase: %v\n", err)
		}
	}()

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
		if dt.Status != store.TaskVerified {
			return fail(fmt.Errorf("dependency %s is %s, not verified; run it first", deps[0], dt.Status))
		}
		db, err := s.GetBranch(ctx, deps[0])
		if err != nil {
			return fail(fmt.Errorf("dependency %s is verified but has no branch: %w", deps[0], err))
		}
		newBase = db.Name
		if newTip, err = gitx.RevParse(ctx, paths.Root, db.Name); err != nil {
			return fail(fmt.Errorf("dependency branch %q: %w", db.Name, err))
		}
	default:
		return fail(fmt.Errorf("task %s has %d dependencies (%s): rebasing fan-in tasks is not supported yet", id, len(deps), strings.Join(deps, ", ")))
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
