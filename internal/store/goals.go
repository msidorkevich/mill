package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mill/internal/budget"
	"mill/internal/plan"
	"mill/internal/taskspec"
)

// Goal statuses.
const (
	GoalDraft    = "draft"
	GoalApproved = "approved"
	GoalRunning  = "running"
	GoalDone     = "done"
	GoalBlocked  = "blocked"
	GoalLanded   = "landed" // every task has landed on the base branch
)

// GoalStatuses lists every valid goal status.
var GoalStatuses = []string{GoalDraft, GoalApproved, GoalRunning, GoalDone, GoalBlocked, GoalLanded}

// Goal is a request broken into a plan.
type Goal struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Status    string          `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	Plan      json.RawMessage `json:"plan"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// TaskID returns the full id of a goal's task: "g1/a".
func TaskID(goalID, local string) string { return goalID + "/" + local }

// SplitTaskID splits a full task id into goal and local parts; ids without a slash have no goal.
func SplitTaskID(id string) (goalID, local string) {
	i := strings.Index(id, "/")
	if i < 0 {
		return "", id
	}
	return id[:i], id[i+1:]
}

// appendEventTx appends an event through tx. The store has one connection, so code inside a
// transaction must not use s.db or s.AppendEvent.
func (s *Store) appendEventTx(ctx context.Context, tx *sql.Tx, kind, goalID, taskID string, data any) error {
	payload := []byte("{}")
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal event data: %w", err)
		}
		payload = b
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (at, kind, goal_id, task_id, data) VALUES (?, ?, ?, ?, ?)`,
		s.ts(), kind, nullable(goalID), nullable(taskID), string(payload)); err != nil {
		return fmt.Errorf("append event %s: %w", kind, err)
	}
	return nil
}

func planOrEmpty(p json.RawMessage) string {
	if len(p) == 0 {
		return "{}"
	}
	return string(p)
}

// CreateGoal stores a new draft goal with the next id (g1, g2, ...) and records goal-created.
func (s *Store) CreateGoal(ctx context.Context, title string, p json.RawMessage) (Goal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Goal{}, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM goals`)
	if err != nil {
		return Goal{}, err
	}
	max := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Goal{}, err
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "g")); err == nil && strings.HasPrefix(id, "g") && n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Goal{}, err
	}
	rows.Close()

	id := "g" + strconv.Itoa(max+1)
	now := s.ts()
	if _, err := tx.ExecContext(ctx, `INSERT INTO goals (id, title, status, plan, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, title, GoalDraft, planOrEmpty(p), now, now); err != nil {
		return Goal{}, fmt.Errorf("create goal %s: %w", id, err)
	}
	if err := s.appendEventTx(ctx, tx, "goal-created", id, "", map[string]string{"title": title}); err != nil {
		return Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return Goal{}, fmt.Errorf("create goal %s: %w", id, err)
	}
	return s.GetGoal(ctx, id)
}

const goalCols = `id, title, status, reason, plan, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanGoal(r rowScanner) (Goal, error) {
	var g Goal
	var p, created, updated string
	if err := r.Scan(&g.ID, &g.Title, &g.Status, &g.Reason, &p, &created, &updated); err != nil {
		return Goal{}, err
	}
	g.Plan = json.RawMessage(p)
	var err error
	if g.CreatedAt, err = parseTime(created); err != nil {
		return Goal{}, fmt.Errorf("goal %s: bad created_at %q: %w", g.ID, created, err)
	}
	if g.UpdatedAt, err = parseTime(updated); err != nil {
		return Goal{}, fmt.Errorf("goal %s: bad updated_at %q: %w", g.ID, updated, err)
	}
	return g, nil
}

// GetGoal loads a goal.
func (s *Store) GetGoal(ctx context.Context, id string) (Goal, error) {
	g, err := scanGoal(s.db.QueryRowContext(ctx, `SELECT `+goalCols+` FROM goals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	return g, err
}

// ListGoals returns all goals, oldest first.
func (s *Store) ListGoals(ctx context.Context) ([]Goal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+goalCols+` FROM goals ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGoalStatus changes a goal's status and reason and records a goal-status event.
func (s *Store) SetGoalStatus(ctx context.Context, id, status, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE goals SET status = ?, reason = ?, updated_at = ? WHERE id = ?`,
		status, reason, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	if err := s.appendEventTx(ctx, tx, "goal-status", id, "",
		map[string]string{"status": status, "reason": reason}); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateGoalPlan replaces a draft goal's plan and records goal-plan-updated.
func (s *Store) UpdateGoalPlan(ctx context.Context, id string, p json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM goals WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if status != GoalDraft {
		return fmt.Errorf("goal %s is %s: the plan is already approved and can no longer be changed", id, status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET plan = ?, updated_at = ? WHERE id = ?`,
		planOrEmpty(p), s.ts(), id); err != nil {
		return err
	}
	if err := s.appendEventTx(ctx, tx, "goal-plan-updated", id, "", nil); err != nil {
		return err
	}
	return tx.Commit()
}

// ApproveGoal stores p as the draft goal's plan, creates its tasks and dependencies and sets
// the goal approved, all in one transaction.
func (s *Store) ApproveGoal(ctx context.Context, id string, p plan.Plan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM goals WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if status != GoalDraft {
		return fmt.Errorf("goal %s is %s, not draft: only a draft goal can be approved", id, status)
	}
	now := s.ts()
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET plan = ?, status = ?, reason = '', updated_at = ? WHERE id = ?`,
		string(raw), GoalApproved, now, id); err != nil {
		return err
	}
	if err := s.appendEventTx(ctx, tx, "goal-status", id, "",
		map[string]string{"status": GoalApproved, "reason": ""}); err != nil {
		return err
	}
	for _, t := range p.Tasks {
		full := TaskID(id, t.ID)
		acc, _ := json.Marshal(t.Acceptance)
		chk, _ := json.Marshal(t.Checks)
		own, _ := json.Marshal(t.Owns)
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id, goal_id, title, kind, spec, acceptance, checks, owns, size, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			full, id, t.Title, t.Kind, t.Spec, string(acc), string(chk), string(own), t.Size,
			TaskPending, now, now); err != nil {
			return fmt.Errorf("create task %s: %w", full, err)
		}
		if err := s.appendEventTx(ctx, tx, "task-created", id, full, map[string]string{"title": t.Title}); err != nil {
			return err
		}
	}
	// Dependencies after all tasks exist, so plan order does not matter for foreign keys.
	for _, t := range p.Tasks {
		for _, d := range t.DependsOn {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deps (task_id, depends_on) VALUES (?, ?)`,
				TaskID(id, t.ID), TaskID(id, d)); err != nil {
				return fmt.Errorf("add dependency %s -> %s: %w", t.ID, d, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("approve goal %s: %w", id, err)
	}
	return nil
}

// GoalTasks returns a goal's tasks in plan (insertion) order.
func (s *Store) GoalTasks(ctx context.Context, goalID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE goal_id = ? ORDER BY rowid`, goalID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []Task
	for _, id := range ids {
		t, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// TaskDeps returns a task's direct dependencies (full ids) in plan order.
func (s *Store) TaskDeps(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT depends_on FROM deps WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GoalUsage sums what all attempts of the goal's tasks spent.
func (s *Store) GoalUsage(ctx context.Context, goalID string) (budget.Usage, error) {
	var u budget.Usage
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.input_tokens),0), COALESCE(SUM(a.output_tokens),0),
		COALESCE(SUM(a.cache_write_tokens),0), COALESCE(SUM(a.cache_read_tokens),0), COALESCE(SUM(a.cost_usd),0)
		FROM attempts a JOIN tasks t ON t.id = a.task_id WHERE t.goal_id = ?`, goalID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CacheWriteTokens, &u.CacheReadTokens, &u.CostUSD)
	return u, err
}

// ResetGoalTasks puts a goal's tasks back to pending with the given reason, in one
// transaction, and records a task-status event for each. A task is reset when its status is
// one of statuses and, if onlyReason is not empty, its reason equals onlyReason. It returns
// the reset task ids in plan order.
func (s *Store) ResetGoalTasks(ctx context.Context, goalID string, statuses []string, onlyReason, reason string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reset tasks of goal %s: %w", goalID, err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, status, reason FROM tasks WHERE goal_id = ? ORDER BY rowid`, goalID)
	if err != nil {
		return nil, fmt.Errorf("reset tasks of goal %s: %w", goalID, err)
	}
	var ids []string
	for rows.Next() {
		var id, status, why string
		if err := rows.Scan(&id, &status, &why); err != nil {
			rows.Close()
			return nil, err
		}
		match := false
		for _, st := range statuses {
			if status == st {
				match = true
			}
		}
		if match && (onlyReason == "" || why == onlyReason) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	now := s.ts()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, phase = '', reason = ?, updated_at = ? WHERE id = ?`,
			TaskPending, reason, now, id); err != nil {
			return nil, fmt.Errorf("reset task %s: %w", id, err)
		}
		if err := s.appendEventTx(ctx, tx, "task-status", goalID, id,
			map[string]string{"status": TaskPending, "phase": "", "reason": reason}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reset tasks of goal %s: %w", goalID, err)
	}
	return ids, nil
}

// UpdateGoalTaskSpec replaces a goal task's spec in the task row and in the goal's stored plan,
// and records task-spec-updated, all in one transaction. The id and the dependencies of the
// task are not changed: the plan's depends_on for the task is kept as stored.
func (s *Store) UpdateGoalTaskSpec(ctx context.Context, taskID string, spec taskspec.Spec) error {
	goalID, local := SplitTaskID(taskID)
	if goalID == "" {
		return fmt.Errorf("task %s is not a goal task: use UpdateTaskSpec", taskID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRowContext(ctx, `SELECT plan FROM goals WHERE id = ?`, goalID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("goal %s: %w", goalID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	p, err := plan.Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("stored plan of %s is unreadable: %w", goalID, err)
	}
	found := false
	for i := range p.Tasks {
		if p.Tasks[i].ID != local {
			continue
		}
		spec.ID = local
		spec.DependsOn = p.Tasks[i].DependsOn
		p.Tasks[i] = spec
		found = true
	}
	if !found {
		return fmt.Errorf("task %s is not in the stored plan of %s: %w", taskID, goalID, ErrNotFound)
	}
	planJSON, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	now := s.ts()
	acc, _ := json.Marshal(spec.Acceptance)
	chk, _ := json.Marshal(spec.Checks)
	own, _ := json.Marshal(spec.Owns)
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET title = ?, kind = ?, spec = ?, acceptance = ?,
		checks = ?, owns = ?, size = ?, updated_at = ? WHERE id = ?`,
		spec.Title, spec.Kind, spec.Spec, string(acc), string(chk), string(own), spec.Size, now, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET plan = ?, updated_at = ? WHERE id = ?`,
		string(planJSON), now, goalID); err != nil {
		return err
	}
	if err := s.appendEventTx(ctx, tx, "task-spec-updated", goalID, taskID, nil); err != nil {
		return err
	}
	return tx.Commit()
}
