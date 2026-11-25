package pipeline

import (
	"context"
	"fmt"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
)

// siblingOwners maps each of the files outside t's owns to the ids of the other tasks of
// t's goal, not yet landed, whose owns match it, in plan order. Files no such task owns are
// left out; a task outside a goal has no siblings.
func (p *Pipeline) siblingOwners(ctx context.Context, t store.Task, outside []string) (map[string][]string, error) {
	owners := map[string][]string{}
	if t.GoalID == "" || len(outside) == 0 {
		return owners, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, t.GoalID)
	if err != nil {
		return nil, fmt.Errorf("list the tasks of goal %s to find who owns the files outside %s's owns: %w", t.GoalID, t.ID, err)
	}
	for _, f := range outside {
		for _, s := range tasks {
			if s.ID == t.ID || s.Status == store.TaskLanded {
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
