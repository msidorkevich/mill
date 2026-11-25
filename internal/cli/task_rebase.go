package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/fanin"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/syncer"
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

	mr, err := moveTaskOntoBase(ctx, s, cfg, paths, t)
	if mr.Restacked {
		return rebaseFanIn(ctx, env, s, cfg, paths, t, mr, err)
	}
	var ce *moveConflictError
	if errors.As(err, &ce) {
		fmt.Fprintf(env.Stderr, "mill task rebase: rebasing %s onto %s (%s) conflicts; the rebase was aborted and the branch is unchanged.\nConflicting files:\n", id, mr.Onto, short(mr.OntoSHA))
		for _, f := range ce.Files {
			fmt.Fprintf(env.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(env.Stderr, "Resolve by hand in %s, or edit the task and run it again from scratch.\n", mr.Worktree)
		return ExitError
	}
	if err != nil {
		return fail(err)
	}
	if mr.UpToDate {
		fmt.Fprintf(env.Stdout, "%s is already up to date: %s already contains %s (%s). Nothing changed.\n", id, mr.Branch, mr.Onto, short(mr.OntoSHA))
		if t.RestackBlockedFrom != "" {
			return restoreAfterRebase(ctx, env, s, cfg, paths, t, mr.Onto)
		}
		return ExitOK
	}

	fmt.Fprintf(env.Stdout, "Rebased %s onto %s.\n  starting point  %s -> %s\n  head            %s -> %s\n", id, mr.Onto, short(mr.OldStart), short(mr.NewStart), short(mr.OldHead), short(mr.NewHead))
	if t.RestackBlockedFrom != "" {
		return restoreAfterRebase(ctx, env, s, cfg, paths, t, mr.Onto)
	}
	goalID, _ := store.SplitTaskID(id)
	if goalID != "" {
		fmt.Fprintf(env.Stdout, "Next: mill run %s --retry-blocked\n", goalID)
	} else {
		fmt.Fprintln(env.Stdout, "Next: mill task run --file <file> --retry")
	}
	return ExitOK
}

// rebaseFanIn reports the outcome of restacking a task with several dependencies, done by
// moveTaskOntoBase, with the messages `mill task rebase` prints for it.
func rebaseFanIn(ctx context.Context, env Env, s *store.Store, cfg config.Config, paths project.Paths, t store.Task, mr moveResult, err error) int {
	id := t.ID
	var ce *moveConflictError
	if errors.As(err, &ce) {
		fmt.Fprintf(env.Stderr, "mill task rebase: restacking %s onto %s (%s) conflicts at %s; the restack was aborted and the branch is unchanged.\nConflicting files:\n", id, mr.Onto, short(mr.OntoSHA), ce.Step)
		for _, f := range ce.Files {
			fmt.Fprintf(env.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(env.Stderr, "Resolve by hand in %s, or edit the task and run it again from scratch.\n", mr.Worktree)
		return ExitError
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task rebase: %v\n", err)
		return ExitError
	}
	if mr.UpToDate {
		fmt.Fprintf(env.Stdout, "%s is already up to date: %s already contains its dependencies. Nothing changed.\n", id, mr.Branch)
		if t.RestackBlockedFrom != "" {
			return restoreAfterRebase(ctx, env, s, cfg, paths, t, mr.Onto)
		}
		return ExitOK
	}

	merged := "none"
	if len(mr.Merged) > 0 {
		merged = strings.Join(mr.Merged, ", ")
	}
	fmt.Fprintf(env.Stdout, "Restacked %s onto %s and merged %s.\n  starting point  %s -> %s\n  head            %s -> %s\n",
		id, mr.Onto, merged, short(mr.OldStart), short(mr.NewStart), short(mr.OldHead), short(mr.NewHead))
	if t.RestackBlockedFrom != "" {
		return restoreAfterRebase(ctx, env, s, cfg, paths, t, mr.Onto)
	}
	if goalID, _ := store.SplitTaskID(id); goalID != "" {
		fmt.Fprintf(env.Stdout, "Next: mill run %s --retry-blocked\n", goalID)
	}
	return ExitOK
}

// moveResult describes what moveTaskOntoBase did to a task's branch.
type moveResult struct {
	Onto     string // the branch the task was moved onto (its base or a dependency's branch)
	OntoSHA  string // the commit of Onto the task was moved onto
	Branch   string // the task's branch
	Worktree string // the task's worktree
	OldStart string
	NewStart string
	OldHead  string
	NewHead  string
	// Merged lists the dependencies merged into the new starting point of a fan-in task.
	Merged []string
	// UpToDate is set when the branch already contained its base: nothing changed.
	UpToDate bool
	// Restacked is set for a task with several dependencies (a fan-in restack).
	Restacked bool
	// ConflictFiles and ConflictStep are set when the move conflicted; the branch is unchanged.
	ConflictFiles []string
	ConflictStep  string
}

// moveConflictError reports that moving a task onto its base conflicted. The move was
// aborted and the branch is unchanged.
type moveConflictError struct {
	Task  string
	Onto  string
	Files []string
	// Step is the fan-in restack step that conflicted; empty for a plain rebase.
	Step string
}

func (e *moveConflictError) Error() string {
	at := ""
	if e.Step != "" {
		at = " at " + e.Step
	}
	return fmt.Sprintf("moving %s onto %s conflicts%s in %s; the move was aborted and the branch is unchanged", e.Task, e.Onto, at, strings.Join(e.Files, ", "))
}

// depNotReadyError reports that a task cannot be moved because a dependency is not
// verified, published or landed yet.
type depNotReadyError struct {
	Task string
	msg  string
}

func (e *depNotReadyError) Error() string { return e.msg }

// moveTaskOntoBase moves task t's own commits onto its current base: the base branch for a
// task without dependencies, its dependency's branch for one dependency, and a restack onto
// its dependencies (fanin.Plan and fanin.Restack) for several. It records the new branch
// state and appends a task-rebased event. A conflict returns a *moveConflictError and leaves
// the branch unchanged; a dependency that is not done returns a *depNotReadyError. The caller
// holds the task's claim and checks its status.
func moveTaskOntoBase(ctx context.Context, s *store.Store, cfg config.Config, paths project.Paths, t store.Task) (moveResult, error) {
	id := t.ID
	var mr moveResult
	br, err := s.GetBranch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return mr, fmt.Errorf("task %s has no branch yet, so there is nothing to rebase; run it first", id)
	}
	if err != nil {
		return mr, err
	}
	mr.Branch, mr.Worktree = br.Name, br.Worktree
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	if oldStart == "" {
		return mr, fmt.Errorf("task %s has no recorded starting point; cannot tell which commits are its own", id)
	}
	mr.OldStart = oldStart

	deps, err := s.TaskDeps(ctx, id)
	if err != nil {
		return mr, err
	}
	switch len(deps) {
	case 0:
		mr.Onto = cfg.BaseBranch
		if mr.OntoSHA, err = gitx.RevParse(ctx, paths.Root, mr.Onto); err != nil {
			return mr, fmt.Errorf("base branch %q: %w", mr.Onto, err)
		}
	case 1:
		dt, err := s.GetTask(ctx, deps[0])
		if err != nil {
			return mr, err
		}
		if !store.Done(dt.Status) {
			return mr, &depNotReadyError{Task: id, msg: fmt.Sprintf("dependency %s is %s, not verified, published or landed; run it first", deps[0], dt.Status)}
		}
		db, err := s.GetBranch(ctx, deps[0])
		if err != nil {
			return mr, fmt.Errorf("dependency %s is %s but has no branch: %w", deps[0], dt.Status, err)
		}
		mr.Onto = db.Name
		if mr.OntoSHA, err = gitx.RevParse(ctx, paths.Root, db.Name); err != nil {
			return mr, fmt.Errorf("dependency branch %q: %w", db.Name, err)
		}
	default:
		mr.Restacked = true
		return restackTask(ctx, s, cfg, paths, t, br, mr)
	}

	if err := checkMoveWorktree(ctx, br); err != nil {
		return mr, err
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return mr, err
	}
	mr.OldHead = head
	upToDate, err := gitx.IsAncestor(ctx, br.Worktree, mr.OntoSHA, head)
	if err != nil {
		return mr, err
	}
	if upToDate {
		mr.UpToDate, mr.NewStart, mr.NewHead = true, oldStart, head
		return mr, nil
	}

	newHead, err := gitx.ReplayOnto(ctx, br.Worktree, id, mr.OntoSHA, oldStart)
	var ce *gitx.RebaseConflictError
	if errors.As(err, &ce) {
		mr.ConflictFiles = ce.Files
		return mr, &moveConflictError{Task: id, Onto: mr.Onto, Files: ce.Files}
	}
	if err != nil {
		return mr, err
	}

	br.BaseSHA, br.StartSHA, br.HeadSHA = mr.OntoSHA, mr.OntoSHA, newHead
	if err := s.SaveBranch(ctx, br); err != nil {
		return mr, fmt.Errorf("the branch was rebased to %s but recording it failed: %w; fix the problem and run `mill task rebase %s` again", short(newHead), err, id)
	}
	mr.NewStart, mr.NewHead = mr.OntoSHA, newHead
	goalID, _ := store.SplitTaskID(id)
	if _, err := s.AppendEvent(ctx, "task-rebased", goalID, id, map[string]any{"old_start": oldStart, "new_start": mr.OntoSHA}); err != nil {
		return mr, err
	}
	return mr, nil
}

// restackTask moves a task with several dependencies: it resets the branch to its deepest
// dependency (or the remote base branch when that one landed), merges the other dependencies
// that have not landed and replays the task's own commits. A conflict aborts and leaves the
// branch unchanged.
func restackTask(ctx context.Context, s *store.Store, cfg config.Config, paths project.Paths, t store.Task, br store.Branch, mr moveResult) (moveResult, error) {
	id := t.ID
	root := paths.Root
	tg, err := fanin.Plan(ctx, s, cfg, root, t)
	if err != nil {
		return mr, err
	}
	if tg.Wait != "" {
		return mr, &depNotReadyError{Task: id, msg: fmt.Sprintf("task %s cannot be rebased yet: %s; run it first", id, tg.Wait)}
	}
	// Like a task with one dependency, use the dependencies' branches as they are in git.
	for i, p := range tg.Parents {
		if p.Landed {
			continue
		}
		tip, err := gitx.RevParse(ctx, root, p.Branch)
		if err != nil {
			return mr, fmt.Errorf("dependency branch %q: %w", p.Branch, err)
		}
		tg.Parents[i].Head = tip
		if i == 0 {
			tg.BaseSHA = tip
		}
	}
	mr.Onto, mr.OntoSHA = tg.Onto, tg.BaseSHA

	if err := checkMoveWorktree(ctx, br); err != nil {
		return mr, err
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return mr, err
	}
	if head != br.HeadSHA {
		return mr, fmt.Errorf("branch %s is at %s but mill recorded head %s; reset it to the recorded head before rebasing", br.Name, short(head), short(br.HeadSHA))
	}
	mr.OldHead = head
	moved, err := tg.Moved(ctx, root, br)
	if err != nil {
		return mr, err
	}
	if !moved {
		mr.UpToDate, mr.NewStart, mr.NewHead = true, mr.OldStart, head
		return mr, nil
	}

	fr, err := fanin.Restack(ctx, fanin.Options{Branch: br, Target: tg})
	var ce *fanin.ConflictError
	if errors.As(err, &ce) {
		mr.ConflictFiles, mr.ConflictStep = ce.Files, ce.Step
		return mr, &moveConflictError{Task: id, Onto: tg.Onto, Files: ce.Files, Step: ce.Step}
	}
	if err != nil {
		return mr, err
	}

	br.Base, br.BaseSHA, br.StartSHA, br.HeadSHA = tg.Base, tg.BaseSHA, fr.StartSHA, fr.Head
	if err := s.SaveBranch(ctx, br); err != nil {
		return mr, fmt.Errorf("the branch was restacked to %s but recording it failed: %w; reset %s to %s by hand, or record the new head", short(fr.Head), err, br.Name, short(head))
	}
	mr.NewStart, mr.NewHead, mr.Merged = fr.StartSHA, fr.Head, fr.Merged
	goalID, _ := store.SplitTaskID(id)
	if _, err := s.AppendEvent(ctx, "task-rebased", goalID, id, map[string]any{"old_start": mr.OldStart, "new_start": fr.StartSHA, "onto": tg.Onto, "merged": fr.Merged}); err != nil {
		return mr, err
	}
	return mr, nil
}

// checkMoveWorktree refuses to move a branch whose worktree is not on it or has uncommitted
// changes.
func checkMoveWorktree(ctx context.Context, br store.Branch) error {
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return fmt.Errorf("worktree %s is not a checkout of %s; restore it before rebasing", br.Worktree, br.Name)
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree %s has uncommitted changes; commit or discard them, then rebase again", br.Worktree)
	}
	return nil
}

// restoreAfterRebase gives task t, blocked by a restack conflict and now rebased onto onto,
// back the status it had (verified or published), as a sync does after a restack: the gate
// re-runs and a published task is re-published. The caller holds the task's claim.
func restoreAfterRebase(ctx context.Context, env Env, s *store.Store, cfg config.Config, paths project.Paths, t store.Task, onto string) int {
	res, err := syncer.Restore(ctx, syncer.Options{Store: s, Cfg: cfg, Paths: paths, Out: env.Stdout, Home: env.Home}, t.ID, onto)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task rebase: %s was rebased but restoring its status %s failed: %v; run `mill task rebase %s` again\n", t.ID, t.RestackBlockedFrom, err, t.ID)
		return ExitError
	}
	if len(res.Blocked) > 0 {
		fmt.Fprintf(env.Stderr, "mill task rebase: %s was rebased but is blocked again: %s\n", t.ID, res.Blocked[0].Reason)
		return ExitError
	}
	now, err := s.GetTask(ctx, t.ID)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task rebase: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "%s is %s again; it was blocked only because a restack conflicted.\n", t.ID, now.Status)
	return ExitOK
}
