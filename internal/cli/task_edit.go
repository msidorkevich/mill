package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"mill/internal/store"
	"mill/internal/taskspec"
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
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

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
	if !slices.Equal(spec.DependsOn, shown.DependsOn) {
		errs = append(errs, fmt.Errorf("depends_on must stay %v, got %v: dependencies cannot change after approval", shown.DependsOn, spec.DependsOn))
	}
	if isGoalTask && spec.Kind == "implement" && len(spec.Checks) == 0 {
		errs = append(errs, errors.New("an implement task needs at least one check"))
	}
	if len(errs) > 0 {
		return invalid(errors.Join(errs...))
	}

	if isGoalTask {
		err = s.UpdateGoalTaskSpec(ctx, id, spec)
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
	if isGoalTask {
		fmt.Fprintf(env.Stdout, "\nNext: `mill run %s --retry-blocked`.\n", goalID)
	} else {
		// task run --retry re-reads its file, so it must hold the edited spec.
		fmt.Fprintf(env.Stdout, "\nNext: `mill task run --file %s --retry`.\n", path)
	}
	return ExitOK
}
