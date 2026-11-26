package pipeline

import (
	"context"
	"fmt"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// siblingOwners maps each of the files outside t's owns to the ids of the other tasks of
// t's goal, not yet landed and not depending on t (directly or transitively), whose owns
// match it, in plan order. Files no such task owns are left out; a task outside a goal has
// no siblings. A dependent builds on t's change, so t editing its files is not a conflict.
func (p *Pipeline) siblingOwners(ctx context.Context, t store.Task, outside []string) (map[string][]string, error) {
	owners := map[string][]string{}
	if t.GoalID == "" || len(outside) == 0 {
		return owners, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, t.GoalID)
	if err != nil {
		return nil, fmt.Errorf("list the tasks of goal %s to find who owns the files outside %s's owns: %w", t.GoalID, t.ID, err)
	}
	dependents, err := p.dependents(ctx, t.ID, tasks)
	if err != nil {
		return nil, err
	}
	for _, f := range outside {
		for _, s := range tasks {
			if s.ID == t.ID || s.Status == store.TaskLanded || dependents[s.ID] {
				continue
			}
			for _, o := range s.Owns {
				if plan.MatchOwns(o, f) {
					owners[f] = append(owners[f], s.ID)
					break
				}
			}
		}
	}
	return owners, nil
}

// dependents returns the ids of the tasks among tasks that depend on id, directly or
// transitively.
func (p *Pipeline) dependents(ctx context.Context, id string, tasks []store.Task) (map[string]bool, error) {
	deps := map[string][]string{}
	for _, s := range tasks {
		ds, err := p.Store.TaskDeps(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("list the dependencies of task %s to find the tasks depending on %s: %w", s.ID, id, err)
		}
		deps[s.ID] = ds
	}
	out := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, s := range tasks {
			if out[s.ID] || s.ID == id {
				continue
			}
			for _, d := range deps[s.ID] {
				if d == id || out[d] {
					out[s.ID] = true
					changed = true
					break
				}
			}
		}
	}
	return out, nil
}

// splitOutOfScope splits verifier findings of t into those for t and those about a file
// another task of t's goal owns and t does not. owners maps each out-of-scope file to the
// ids of the tasks owning it, in plan order.
func (p *Pipeline) splitOutOfScope(ctx context.Context, t store.Task, findings []prompt.Finding) (in, out []prompt.Finding, owners map[string][]string, err error) {
	owners = map[string][]string{}
	if t.GoalID == "" {
		return findings, nil, owners, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, t.GoalID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list the tasks of goal %s to scope the verifier findings of %s: %w", t.GoalID, t.ID, err)
	}
	for _, f := range findings {
		if f.File == "" || len(plan.OutsideOwns(t.Owns, []string{f.File})) == 0 {
			in = append(in, f)
			continue
		}
		var ids []string
		for _, s := range tasks {
			if s.ID == t.ID {
				continue
			}
			for _, o := range s.Owns {
				if plan.MatchOwns(o, f.File) {
					ids = append(ids, s.ID)
					break
				}
			}
		}
		if len(ids) == 0 {
			in = append(in, f)
			continue
		}
		out = append(out, f)
		owners[f.File] = ids
	}
	return in, out, owners, nil
}
