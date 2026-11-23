package store

import (
	"context"
	"fmt"
)

// Schedule is the work the scheduler may run.
type Schedule struct {
	// Goals are the approved, running and blocked goals, and done goals with an unfinished
	// task, oldest first. Draft and landed goals are never listed.
	Goals []Goal
	// Tasks are every task of Goals in plan order, followed by the pending single tasks (tasks
	// without a goal), oldest first.
	Tasks []Task
	// Deps are the direct dependencies of each task in Tasks, in plan order.
	Deps map[string][]string
}

// ScheduleCandidates reads the goals and tasks the scheduler may run. It changes nothing.
func (s *Store) ScheduleCandidates(ctx context.Context) (Schedule, error) {
	goals, err := s.ListGoals(ctx)
	if err != nil {
		return Schedule{}, fmt.Errorf("list goals to schedule: %w", err)
	}
	out := Schedule{Deps: map[string][]string{}}
	for _, g := range goals {
		switch g.Status {
		case GoalApproved, GoalRunning, GoalBlocked, GoalDone:
		default:
			continue
		}
		tasks, err := s.GoalTasks(ctx, g.ID)
		if err != nil {
			return Schedule{}, fmt.Errorf("list tasks of goal %s: %w", g.ID, err)
		}
		if g.Status == GoalDone {
			unfinished := false
			for _, t := range tasks {
				if !Done(t.Status) {
					unfinished = true
					break
				}
			}
			if !unfinished {
				continue
			}
		}
		out.Goals = append(out.Goals, g)
		out.Tasks = append(out.Tasks, tasks...)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE (goal_id IS NULL OR goal_id = '') AND status = ?
		ORDER BY rowid`, TaskPending)
	if err != nil {
		return Schedule{}, fmt.Errorf("list pending single tasks: %w", err)
	}
	var single []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Schedule{}, fmt.Errorf("list pending single tasks: %w", err)
		}
		single = append(single, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Schedule{}, fmt.Errorf("list pending single tasks: %w", err)
	}
	rows.Close()
	for _, id := range single {
		t, err := s.GetTask(ctx, id)
		if err != nil {
			return Schedule{}, err
		}
		out.Tasks = append(out.Tasks, t)
	}

	listed := map[string]bool{}
	for _, t := range out.Tasks {
		listed[t.ID] = true
	}
	rows, err = s.db.QueryContext(ctx, `SELECT task_id, depends_on FROM deps ORDER BY rowid`)
	if err != nil {
		return Schedule{}, fmt.Errorf("list task dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, dep string
		if err := rows.Scan(&id, &dep); err != nil {
			return Schedule{}, fmt.Errorf("list task dependencies: %w", err)
		}
		if listed[id] {
			out.Deps[id] = append(out.Deps[id], dep)
		}
	}
	if err := rows.Err(); err != nil {
		return Schedule{}, fmt.Errorf("list task dependencies: %w", err)
	}
	return out, nil
}
