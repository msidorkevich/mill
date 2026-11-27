package pipeline

import (
	"context"
	"fmt"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// siblingOwners maps each of the files outside t's owns to the ids of the other tasks of
// t's goal, not yet landed and neither depending on t nor depended on by t (directly or
// transitively), whose owns match it, in plan order. Files no such task owns are left out; a
// task outside a goal has no siblings. A dependent builds on t's change and t builds on a
// dependency's, so t editing the files of either is not a conflict.
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
	dependencies, err := p.dependencies(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range outside {
		for _, s := range tasks {
			if s.ID == t.ID || s.Status == store.TaskLanded || dependents[s.ID] || dependencies[s.ID] {
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

// dependencies returns the ids of the tasks id depends on, directly or transitively.
func (p *Pipeline) dependencies(ctx context.Context, id string) (map[string]bool, error) {
	out := map[string]bool{}
	for queue := []string{id}; len(queue) > 0; queue = queue[1:] {
		ds, err := p.Store.TaskDeps(ctx, queue[0])
		if err != nil {
			return nil, fmt.Errorf("list the dependencies of task %s to find the tasks %s depends on: %w", queue[0], id, err)
		}
		for _, d := range ds {
			if !out[d] {
				out[d] = true
				queue = append(queue, d)
			}
		}
	}
	return out, nil
}

// splitOutOfScope splits verifier findings of t into those for t and those about a file
// another task of t's goal, not yet landed, owns and t does not. owners maps each
// out-of-scope file to the ids of those tasks, in plan order. changed lists the files
// outside t's owns that t's attempt changed: a finding about one of them that a not yet
// landed dependency of t owns stays with t, since t's edit to it was exempt from the owns
// check and only the verifier can still reject it.
func (p *Pipeline) splitOutOfScope(ctx context.Context, t store.Task, findings []prompt.Finding, changed []string) (in, out []prompt.Finding, owners map[string][]string, err error) {
	owners = map[string][]string{}
	if t.GoalID == "" {
		return findings, nil, owners, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, t.GoalID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list the tasks of goal %s to scope the verifier findings of %s: %w", t.GoalID, t.ID, err)
	}
	var dependencies map[string]bool
	if len(changed) > 0 {
		if dependencies, err = p.dependencies(ctx, t.ID); err != nil {
			return nil, nil, nil, err
		}
	}
	touched := map[string]bool{}
	for _, f := range changed {
		touched[f] = true
	}
	for _, f := range findings {
		if f.File == "" || len(plan.OutsideOwns(t.Owns, []string{f.File})) == 0 {
			in = append(in, f)
			continue
		}
		var ids []string
		mine := false
		for _, s := range tasks {
			if s.ID == t.ID || s.Status == store.TaskLanded {
				continue
			}
			if touched[f.File] && dependencies[s.ID] && ownsFile(s, f.File) {
				mine = true
				break
			}
			for _, o := range s.Owns {
				if plan.MatchOwns(o, f.File) {
					ids = append(ids, s.ID)
					break
				}
			}
		}
		if mine || len(ids) == 0 {
			in = append(in, f)
			continue
		}
		out = append(out, f)
		owners[f.File] = ids
	}
	return in, out, owners, nil
}

// ownsFile reports whether one of s's owns matches file.
func ownsFile(s store.Task, file string) bool {
	for _, o := range s.Owns {
		if plan.MatchOwns(o, file) {
			return true
		}
	}
	return false
}
