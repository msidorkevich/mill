package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/plan"
)

// AddTaskDeps adds dependencies (full task ids) to a goal task whose status is pending,
// blocked or failed, in one transaction: the deps rows go after the existing ones, the
// goal's stored plan lists them after the task's existing depends_on, and a task-deps-added
// event is recorded. Each dependency must be another task of the same goal that the task does
// not depend on yet, and the new edges must not create a cycle. Removal is not supported.
func (s *Store) AddTaskDeps(ctx context.Context, taskID string, add []string) error {
	goalID, local := SplitTaskID(taskID)
	if goalID == "" {
		return fmt.Errorf("task %s is not a goal task: only goal tasks have dependencies", taskID)
	}
	if len(add) == 0 {
		return fmt.Errorf("add dependencies to %s: no dependencies given", taskID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ? AND goal_id = ?`, taskID, goalID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	switch status {
	case TaskPending, TaskBlocked, TaskFailed:
	default:
		return fmt.Errorf("task %s is %s: dependencies can only be added to a pending, blocked or failed task", taskID, status)
	}

	tasks := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE goal_id = ?`, goalID)
	if err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tasks[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// The goal's dependency graph, each task's dependencies in insertion order.
	graph := map[string][]string{}
	rows, err = tx.QueryContext(ctx, `SELECT d.task_id, d.depends_on FROM deps d
		JOIN tasks t ON t.id = d.task_id WHERE t.goal_id = ? ORDER BY d.rowid`, goalID)
	if err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return err
		}
		graph[from] = append(graph[from], to)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	existing := graph[taskID]
	have := map[string]bool{}
	for _, d := range existing {
		have[d] = true
	}
	for _, d := range add {
		dg, _ := SplitTaskID(d)
		switch {
		case d == taskID:
			return fmt.Errorf("task %s cannot depend on itself", taskID)
		case dg != goalID || !tasks[d]:
			return fmt.Errorf("dependency %s is not a task of goal %s: %w", d, goalID, ErrNotFound)
		case have[d]:
			return fmt.Errorf("task %s already depends on %s", taskID, d)
		}
		have[d] = true
	}
	graph[taskID] = append(append([]string{}, existing...), add...)
	if cyc := findCycleFrom(graph, taskID); cyc != nil {
		for i, id := range cyc {
			_, cyc[i] = SplitTaskID(id)
		}
		return fmt.Errorf("adding dependencies to %s would create a dependency cycle: %s",
			taskID, strings.Join(cyc, " -> "))
	}

	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT plan FROM goals WHERE id = ?`, goalID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("goal %s: %w", goalID, ErrNotFound)
		}
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	p, err := plan.Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("stored plan of %s is unreadable: %w", goalID, err)
	}
	var addedLocal, dependsOn []string
	for _, d := range graph[taskID] {
		_, l := SplitTaskID(d)
		dependsOn = append(dependsOn, l)
	}
	for _, d := range add {
		_, l := SplitTaskID(d)
		addedLocal = append(addedLocal, l)
	}
	found := false
	for i := range p.Tasks {
		if p.Tasks[i].ID == local {
			p.Tasks[i].DependsOn = dependsOn
			found = true
		}
	}
	if !found {
		return fmt.Errorf("task %s is not in the stored plan of %s: %w", taskID, goalID, ErrNotFound)
	}
	planJSON, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}

	for _, d := range add {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deps (task_id, depends_on) VALUES (?, ?)`, taskID, d); err != nil {
			return fmt.Errorf("add dependency %s -> %s: %w", taskID, d, err)
		}
	}
	now := s.ts()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, now, taskID); err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET plan = ?, updated_at = ? WHERE id = ?`,
		string(planJSON), now, goalID); err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	if err := s.appendEventTx(ctx, tx, "task-deps-added", goalID, taskID,
		map[string][]string{"added": addedLocal, "depends_on": dependsOn}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("add dependencies to %s: %w", taskID, err)
	}
	return nil
}

// findCycleFrom returns a cycle reachable from start in graph (task -> its dependencies),
// such as [a b a], or nil.
func findCycleFrom(graph map[string][]string, start string) []string {
	state := map[string]int{} // 0 new, 1 on stack, 2 done
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = 1
		stack = append(stack, id)
		for _, d := range graph[id] {
			switch state[d] {
			case 1:
				for k, s := range stack {
					if s == d {
						return append(append([]string{}, stack[k:]...), d)
					}
				}
			case 0:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	return visit(start)
}
