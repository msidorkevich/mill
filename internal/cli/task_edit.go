package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func runTaskEdit(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "task edit")
	file := fs.String("file", "", "edited task spec (JSON); without it $EDITOR opens the current spec")
	id, code, ok := goalArg(env, "task edit", "mill task edit <id> [--file spec.json]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task edit: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	// Hold the task's claim for the whole edit, so a running `mill run` cannot start it
	// while its spec, dependencies and branch change.
	release, err := claimTasks(ctx, env, s, "task edit", id)
	if err != nil {
		return fail(err)
	}
	defer release()

	cur, err := s.GetTask(ctx, id)
	if err != nil {
		return fail(err)
	}
	switch cur.Status {
	case store.TaskPending, store.TaskBlocked, store.TaskFailed:
	default:
		return fail(fmt.Errorf("task %s is %s: only a pending, blocked or failed task can be edited", id, cur.Status))
	}
	goalID, local := store.SplitTaskID(id)
	isGoalTask := goalID != ""

	// The spec as the user sees it: local ids for a goal task's dependencies.
	shown := cur.Spec
	shown.ID = local
	shown.DependsOn = []string{}
	if isGoalTask {
		deps, err := s.TaskDeps(ctx, id)
		if err != nil {
			return fail(err)
		}
		for _, d := range deps {
			_, l := store.SplitTaskID(d)
			shown.DependsOn = append(shown.DependsOn, l)
		}
	}
	shown.ApplyDefaults()

	path := *file
	temp := false
	if path == "" {
		data, err := json.MarshalIndent(shown, "", "  ")
		if err != nil {
			return fail(err)
		}
		path, err = editTemp(ctx, env, "mill-task-"+strings.ReplaceAll(id, "/", "-"), data)
		if err != nil {
			return fail(fmt.Errorf("%w; the task was not changed", err))
		}
		temp = true
	} else {
		path = absPath(env, path)
	}
	invalid := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task edit: the spec in %s is not valid; %s is unchanged:\n", path, id)
		problems(env.Stderr, "", err)
		if temp {
			fmt.Fprintf(env.Stderr, "Your edit is kept in %s\nFix it, then run `mill task edit %s --file %s`.\n", path, id, path)
		}
		return ExitError
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var spec taskspec.Spec
	if err := dec.Decode(&spec); err != nil {
		return invalid(fmt.Errorf("parse task: %w", err))
	}
	if dec.More() {
		return invalid(errors.New("parse task: trailing data after the task object"))
	}
	spec.ApplyDefaults()

	var errs []error
	if err := spec.ValidateFields(); err != nil {
		errs = append(errs, err)
	}
	if spec.ID != local {
		errs = append(errs, fmt.Errorf("id must stay %q, got %q: a task's id cannot change", local, spec.ID))
	}
	added, depErrs := addedDeps(shown.DependsOn, spec.DependsOn, isGoalTask)
	errs = append(errs, depErrs...)
	if isGoalTask && spec.Kind == "implement" && len(spec.Checks) == 0 {
		errs = append(errs, errors.New("an implement task needs at least one check"))
	}
	if len(errs) > 0 {
		return invalid(errors.Join(errs...))
	}

	var addedIDs []string
	for _, l := range added {
		addedIDs = append(addedIDs, goalID+"/"+l)
	}
	// Add the dependencies first: their checks (unknown task, cycle) run in AddTaskDeps, and a
	// refusal there must leave the task unchanged.
	if len(addedIDs) > 0 {
		if err := s.AddTaskDeps(ctx, id, addedIDs); err != nil {
			return invalid(err)
		}
	}
	if isGoalTask {
		err = s.UpdateGoalTaskSpec(ctx, id, spec)
		if err != nil && len(addedIDs) > 0 {
			err = fmt.Errorf("dependencies %s were added to %s but updating its spec failed: %w; run `mill task edit %s` again to apply the rest of the edit", strings.Join(addedIDs, ", "), id, err, id)
		}
	} else {
		spec.DependsOn = nil
		spec.ApplyDefaults()
		err = s.UpdateTaskSpec(ctx, spec)
	}
	if err != nil {
		return fail(err)
	}
	if temp && isGoalTask {
		os.Remove(path)
	}
	fmt.Fprintf(env.Stdout, "Updated %s.\n", id)
	if len(addedIDs) > 0 {
		if code := moveAfterAddedDeps(ctx, env, s, cfg, paths, id, addedIDs); code != ExitOK {
			return code
		}
	}
	if isGoalTask {
		fmt.Fprintf(env.Stdout, "\nNext: `mill run %s --retry-blocked`.\n", goalID)
	} else {
		// task run --retry re-reads its file, so it must hold the edited spec.
		fmt.Fprintf(env.Stdout, "\nNext: `mill task run --file %s --retry`.\n", path)
	}
	return ExitOK
}

// addedDeps compares an edited depends_on (want) with the current one (have, local ids).
// The current dependencies must stay, in their order, as a prefix; the entries after them
// are additions, which only a goal task can have.
func addedDeps(have, want []string, isGoalTask bool) (added []string, errs []error) {
	var changed []string
	for i, d := range have {
		if i >= len(want) || want[i] != d {
			changed = append(changed, d)
		}
	}
	if len(changed) > 0 {
		return nil, []error{fmt.Errorf("depends_on cannot drop or reorder %s: published stacks and branches depend on them; only adding dependencies is allowed", strings.Join(changed, ", "))}
	}
	added = want[len(have):]
	if len(added) > 0 && !isGoalTask {
		return nil, []error{fmt.Errorf("depends_on cannot list %s: a standalone task has no dependencies", strings.Join(added, ", "))}
	}
	return added, nil
}

// moveAfterAddedDeps reports the dependencies added to task id and, when the task has a
// branch, moves it onto its new base. A conflict blocks the task. The caller holds the
// task's claim.
func moveAfterAddedDeps(ctx context.Context, env Env, s *store.Store, cfg config.Config, paths project.Paths, id string, added []string) int {
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task edit: %v\n", err)
		return ExitError
	}
	ids := strings.Join(added, ", ")
	fmt.Fprintf(env.Stdout, "Added dependencies %s to %s.\n", ids, id)
	if _, err := s.GetBranch(ctx, id); errors.Is(err, store.ErrNotFound) {
		return ExitOK
	} else if err != nil {
		return fail(fmt.Errorf("dependencies %s were added to %s but its branch cannot be read: %w; run `mill task rebase %s` to move it", ids, id, err, id))
	}
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return fail(fmt.Errorf("dependencies %s were added to %s but it cannot be reloaded: %w; run `mill task rebase %s` to move its branch", ids, id, err, id))
	}

	mr, err := moveTaskOntoBase(ctx, s, cfg, paths, t)
	var dn *depNotReadyError
	var ce *moveConflictError
	switch {
	case errors.As(err, &dn):
		fmt.Fprintf(env.Stdout, "The dependency was recorded, but the branch was not moved: %v.\nThe branch moves when you run `mill task rebase %s` after %s is verified.\n", err, id, ids)
		return ExitOK
	case errors.As(err, &ce):
		reason := fmt.Sprintf("dependencies %s were added, but moving the branch onto its new base %s (%s) conflicts in %s; resolve by hand in %s, or edit the task and run it again from scratch",
			ids, mr.Onto, short(mr.OntoSHA), strings.Join(ce.Files, ", "), mr.Worktree)
		if err := s.SetTaskStatus(ctx, id, store.TaskBlocked, "", reason); err != nil {
			return fail(fmt.Errorf("%s; and blocking %s failed: %w", reason, id, err))
		}
		fmt.Fprintf(env.Stderr, "mill task edit: %s is blocked: %s.\n", id, reason)
		return ExitError
	case err != nil:
		return fail(fmt.Errorf("dependencies %s were added to %s but moving its branch failed: %w; fix the problem and run `mill task rebase %s`", ids, id, err, id))
	}
	if mr.UpToDate {
		fmt.Fprintf(env.Stdout, "%s already contains %s (%s); the branch is unchanged.\n", mr.Branch, mr.Onto, short(mr.OntoSHA))
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "Moved %s onto %s.\n  starting point  %s -> %s\n  head            %s -> %s\n",
		mr.Branch, mr.Onto, short(mr.OldStart), short(mr.NewStart), short(mr.OldHead), short(mr.NewHead))
	return ExitOK
}
