package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/msidorkevich/mill/internal/budget"
)

// ReviewTask is a task awaiting review (verified) or a merge (published).
type ReviewTask struct {
	TaskSummary
	PublishedRef    string `json:"published_ref,omitempty"`
	Approved        bool   `json:"approved,omitempty"`
	ApprovedAt      string `json:"approved_at,omitempty"`
	ApprovedAttempt int    `json:"approved_attempt,omitempty"`
}

// ReviewTasks lists verified and published tasks, oldest first (created_at, then id).
func (s *Store) ReviewTasks(ctx context.Context) ([]ReviewTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.title, t.status, t.phase,
		COALESCE(MAX(a.n), 0), COALESCE(SUM(a.input_tokens), 0), COALESCE(SUM(a.output_tokens), 0),
		COALESCE(SUM(a.cache_write_tokens), 0), COALESCE(SUM(a.cache_read_tokens), 0), COALESCE(SUM(a.cost_usd), 0),
		COALESCE((SELECT b.published_ref FROM branches b WHERE b.task_id = t.id), ''),
		t.approved_at, t.approved_attempt
		FROM tasks t LEFT JOIN attempts a ON a.task_id = t.id
		WHERE t.status IN (?, ?)
		GROUP BY t.id ORDER BY t.created_at, t.id`, TaskVerified, TaskPublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewTask
	for rows.Next() {
		var t ReviewTask
		var u budget.Usage
		var approvedAt sql.NullString
		var approvedAttempt sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.Phase, &t.Attempts,
			&u.InputTokens, &u.OutputTokens, &u.CacheWriteTokens, &u.CacheReadTokens, &u.CostUSD, &t.PublishedRef,
			&approvedAt, &approvedAttempt); err != nil {
			return nil, err
		}
		t.Approved = approvedAt.Valid
		t.ApprovedAt = approvedAt.String
		t.ApprovedAttempt = int(approvedAttempt.Int64)
		t.Usage = u
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskDescendants returns every task that depends on taskID, directly or transitively, in
// breadth-first order without duplicates.
func (s *Store) TaskDescendants(ctx context.Context, taskID string) ([]string, error) {
	seen := map[string]bool{taskID: true}
	var out []string
	queue := []string{taskID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM deps WHERE depends_on = ? ORDER BY rowid`, cur)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
				queue = append(queue, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// RejectTask blocks a task with reason and puts every descendant that is not landed on
// hold (blocked, reason "on hold: dependency <id> was rejected"), in one transaction with a
// task-status event for each. It returns the ids put on hold.
func (s *Store) RejectTask(ctx context.Context, id, reason string) ([]string, error) {
	desc, err := s.TaskDescendants(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reject %s: %w", id, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reject %s: %w", id, err)
	}
	defer tx.Rollback()
	now := s.ts()
	set := func(tid, why string) error {
		var g string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(goal_id, '') FROM tasks WHERE id = ?`, tid).Scan(&g); err != nil {
			return fmt.Errorf("task %s: %w", tid, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, phase = '', reason = ?, updated_at = ? WHERE id = ?`,
			TaskBlocked, why, now, tid); err != nil {
			return err
		}
		if err := s.appendEventTx(ctx, tx, "task-status", g, tid,
			map[string]string{"status": TaskBlocked, "phase": "", "reason": why}); err != nil {
			return err
		}
		return s.clearApprovalTx(ctx, tx, g, tid, why)
	}
	if err := set(id, reason); err != nil {
		return nil, fmt.Errorf("reject %s: %w", id, err)
	}
	var held []string
	for _, d := range desc {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, d).Scan(&status); err != nil {
			return nil, fmt.Errorf("reject %s: task %s: %w", id, d, err)
		}
		if status == TaskLanded {
			continue
		}
		if err := set(d, fmt.Sprintf("on hold: dependency %s was rejected", id)); err != nil {
			return nil, fmt.Errorf("reject %s: hold %s: %w", id, d, err)
		}
		held = append(held, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reject %s: %w", id, err)
	}
	return held, nil
}

// RequestChanges records operator findings (an attempt-findings event with the given attempt
// number) and sets the task pending with reason, in one transaction.
func (s *Store) RequestChanges(ctx context.Context, id string, attempt int, findings any, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("request changes on %s: %w", id, err)
	}
	defer tx.Rollback()
	var g string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(goal_id, '') FROM tasks WHERE id = ?`, id).Scan(&g); err != nil {
		return fmt.Errorf("task %s: %w", id, err)
	}
	if err := s.appendEventTx(ctx, tx, "attempt-findings", g, id,
		map[string]any{"attempt": attempt, "findings": findings}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, phase = '', reason = ?, updated_at = ? WHERE id = ?`,
		TaskPending, reason, s.ts(), id); err != nil {
		return err
	}
	if err := s.appendEventTx(ctx, tx, "task-status", g, id,
		map[string]string{"status": TaskPending, "phase": "", "reason": reason}); err != nil {
		return err
	}
	if err := s.clearApprovalTx(ctx, tx, g, id, "changes requested: "+reason); err != nil {
		return err
	}
	return tx.Commit()
}
