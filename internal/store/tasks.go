package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mill/internal/budget"
	"mill/internal/taskspec"
)

// Task statuses (GR5). Only the harness changes them.
const (
	TaskPending  = "pending"
	TaskRunning  = "running"
	TaskVerified = "verified"
	TaskBlocked  = "blocked"
	TaskFailed   = "failed"
)

// Task phases while running.
const (
	PhasePrepare   = "prepare"
	PhaseImplement = "implement"
	PhaseGate      = "gate"
	PhaseVerify    = "verify"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Task is a stored task.
type Task struct {
	taskspec.Spec
	GoalID    string    `json:"goal_id,omitempty"`
	Status    string    `json:"status"`
	Phase     string    `json:"phase,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) ts() string { return s.now().UTC().Format(timeLayout) }

// CreateTask stores a new pending task and records a task-created event.
func (s *Store) CreateTask(ctx context.Context, spec taskspec.Spec) error {
	acc, _ := json.Marshal(spec.Acceptance)
	chk, _ := json.Marshal(spec.Checks)
	own, _ := json.Marshal(spec.Owns)
	now := s.ts()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id, title, kind, spec, acceptance, checks, owns, size, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.ID, spec.Title, spec.Kind, spec.Spec, string(acc), string(chk), string(own), spec.Size,
		TaskPending, now, now)
	if err != nil {
		return fmt.Errorf("create task %s: %w", spec.ID, err)
	}
	_, err = s.AppendEvent(ctx, "task-created", "", spec.ID, map[string]string{"title": spec.Title})
	return err
}

// GetTask loads a task.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var goal sql.NullString
	var acc, chk, own, created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, goal_id, title, kind, spec, acceptance, checks, owns,
		size, status, phase, reason, created_at, updated_at FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &goal, &t.Title, &t.Kind, &t.Spec.Spec, &acc, &chk, &own, &t.Size, &t.Status,
			&t.Phase, &t.Reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, err
	}
	t.GoalID = goal.String
	for _, f := range []struct {
		raw string
		dst *[]string
	}{{acc, &t.Acceptance}, {chk, &t.Checks}, {own, &t.Owns}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return Task{}, fmt.Errorf("task %s: corrupt list: %w", id, err)
		}
	}
	t.CreatedAt, _ = parseTime(created)
	t.UpdatedAt, _ = parseTime(updated)
	return t, nil
}

// SetTaskStatus changes a task's status, phase and reason and records a task-status event.
func (s *Store) SetTaskStatus(ctx context.Context, id, status, phase, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, phase = ?, reason = ?, updated_at = ? WHERE id = ?`,
		status, phase, reason, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	_, err = s.AppendEvent(ctx, "task-status", "", id,
		map[string]string{"status": status, "phase": phase, "reason": reason})
	return err
}

// Attempt is one agent run for a task.
type Attempt struct {
	ID         int64        `json:"id"`
	TaskID     string       `json:"task_id"`
	N          int          `json:"n"`
	Role       string       `json:"role"`
	Status     string       `json:"status"`
	StartedAt  time.Time    `json:"started_at"`
	EndedAt    *time.Time   `json:"ended_at,omitempty"`
	Usage      budget.Usage `json:"usage"`
	OutputPath string       `json:"output_path,omitempty"`
	LogPath    string       `json:"log_path,omitempty"`
}

// Attempt statuses.
const (
	AttemptRunning = "running"
)

// StartAttempt records the start of an agent run and returns its id.
func (s *Store) StartAttempt(ctx context.Context, taskID string, n int, role string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attempts (task_id, n, role, status, started_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, n, role, AttemptRunning, s.ts())
	if err != nil {
		return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
	}
	return res.LastInsertId()
}

// FinishAttempt records how an agent run ended and what it spent.
func (s *Store) FinishAttempt(ctx context.Context, id int64, status string, u budget.Usage, outputPath, logPath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attempts SET status = ?, ended_at = ?,
		input_tokens = ?, output_tokens = ?, cache_write_tokens = ?, cache_read_tokens = ?, cost_usd = ?,
		output_path = ?, log_path = ? WHERE id = ?`,
		status, s.ts(), u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens, u.CostUSD,
		outputPath, logPath, id)
	return err
}

// Attempts lists a task's attempts in order.
func (s *Store) Attempts(ctx context.Context, taskID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, n, role, status, started_at, ended_at,
		input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost_usd, output_path, log_path
		FROM attempts WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var started string
		var ended sql.NullString
		if err := rows.Scan(&a.ID, &a.TaskID, &a.N, &a.Role, &a.Status, &started, &ended,
			&a.Usage.InputTokens, &a.Usage.OutputTokens, &a.Usage.CacheWriteTokens, &a.Usage.CacheReadTokens,
			&a.Usage.CostUSD, &a.OutputPath, &a.LogPath); err != nil {
			return nil, err
		}
		a.StartedAt, _ = parseTime(started)
		if ended.Valid {
			t, _ := parseTime(ended.String)
			a.EndedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TaskUsage sums what all of a task's attempts spent.
func (s *Store) TaskUsage(ctx context.Context, taskID string) (budget.Usage, error) {
	var u budget.Usage
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cost_usd),0)
		FROM attempts WHERE task_id = ?`, taskID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CacheWriteTokens, &u.CacheReadTokens, &u.CostUSD)
	return u, err
}

// MaxAttemptN returns the highest attempt number used by a task (0 if none).
func (s *Store) MaxAttemptN(ctx context.Context, taskID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(n), 0) FROM attempts WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}

// TaskStatuses lists every valid task status.
var TaskStatuses = []string{TaskPending, TaskRunning, TaskVerified, TaskBlocked, TaskFailed}

// TaskSummary is a task with its aggregated attempts and spend.
type TaskSummary struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Status   string       `json:"status"`
	Phase    string       `json:"phase"`
	Attempts int          `json:"attempts"` // highest attempt number used (0 if none)
	Usage    budget.Usage `json:"usage"`
}

// TaskSummaries lists tasks oldest first (created_at, then id) in one query. A non-empty
// status keeps only tasks with that status.
func (s *Store) TaskSummaries(ctx context.Context, status string) ([]TaskSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.title, t.status, t.phase,
		COALESCE(MAX(a.n), 0), COALESCE(SUM(a.input_tokens), 0), COALESCE(SUM(a.output_tokens), 0),
		COALESCE(SUM(a.cache_write_tokens), 0), COALESCE(SUM(a.cache_read_tokens), 0), COALESCE(SUM(a.cost_usd), 0)
		FROM tasks t LEFT JOIN attempts a ON a.task_id = t.id
		WHERE ? = '' OR t.status = ?
		GROUP BY t.id ORDER BY t.created_at, t.id`, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskSummary
	for rows.Next() {
		var t TaskSummary
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.Phase, &t.Attempts,
			&t.Usage.InputTokens, &t.Usage.OutputTokens, &t.Usage.CacheWriteTokens, &t.Usage.CacheReadTokens,
			&t.Usage.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Branch is a task's git branch and worktree.
type Branch struct {
	TaskID       string    `json:"task_id"`
	Name         string    `json:"name"`
	Base         string    `json:"base"`
	BaseSHA      string    `json:"base_sha"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	Worktree     string    `json:"worktree"`
	PublishedRef string    `json:"published_ref,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SaveBranch inserts or updates a task's branch row.
func (s *Store) SaveBranch(ctx context.Context, b Branch) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO branches
		(task_id, name, base, base_sha, head_sha, worktree, published_ref, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET name = excluded.name, base = excluded.base,
		base_sha = excluded.base_sha, head_sha = excluded.head_sha, worktree = excluded.worktree,
		published_ref = excluded.published_ref, updated_at = excluded.updated_at`,
		b.TaskID, b.Name, b.Base, b.BaseSHA, b.HeadSHA, b.Worktree, b.PublishedRef, s.ts())
	return err
}

// GetBranch loads a task's branch row.
func (s *Store) GetBranch(ctx context.Context, taskID string) (Branch, error) {
	var b Branch
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT task_id, name, base, base_sha, head_sha, worktree,
		published_ref, updated_at FROM branches WHERE task_id = ?`, taskID).
		Scan(&b.TaskID, &b.Name, &b.Base, &b.BaseSHA, &b.HeadSHA, &b.Worktree, &b.PublishedRef, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Branch{}, fmt.Errorf("branch for %s: %w", taskID, ErrNotFound)
	}
	b.UpdatedAt, _ = parseTime(updated)
	return b, err
}

// TaskEvents returns a task's events in seq order.
func (s *Store) TaskEvents(ctx context.Context, taskID string) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT seq, at, kind, COALESCE(goal_id, ''), COALESCE(task_id, ''), data
		FROM events WHERE task_id = ? ORDER BY seq`, taskID)
}

// UpdateTaskSpec replaces a task's spec fields (used when the operator edits the task file
// before a retry) and records a task-spec-updated event.
func (s *Store) UpdateTaskSpec(ctx context.Context, spec taskspec.Spec) error {
	acc, _ := json.Marshal(spec.Acceptance)
	chk, _ := json.Marshal(spec.Checks)
	own, _ := json.Marshal(spec.Owns)
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET title = ?, kind = ?, spec = ?, acceptance = ?,
		checks = ?, owns = ?, size = ?, updated_at = ? WHERE id = ?`,
		spec.Title, spec.Kind, spec.Spec, string(acc), string(chk), string(own), spec.Size, s.ts(), spec.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", spec.ID, ErrNotFound)
	}
	_, err = s.AppendEvent(ctx, "task-spec-updated", "", spec.ID, nil)
	return err
}
