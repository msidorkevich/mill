package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// Task statuses (GR5). Only the harness changes them.
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskVerified  = "verified"
	TaskPublished = "published"
	TaskLanded    = "landed"
	TaskBlocked   = "blocked"
	TaskFailed    = "failed"
)

// Done reports whether status is a finished task whose code later tasks may build on:
// verified, published or landed.
func Done(status string) bool {
	switch status {
	case TaskVerified, TaskPublished, TaskLanded:
		return true
	}
	return false
}

// Task phases while running.
const (
	PhasePrepare   = "prepare"
	PhaseImplement = "implement"
	PhaseGate      = "gate"
	PhaseGateWait  = "waiting for gate"
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
	// Approved is true while the operator's approval of the task's current code stands.
	Approved        bool   `json:"approved,omitempty"`
	ApprovedAt      string `json:"approved_at,omitempty"`
	ApprovedAttempt int    `json:"approved_attempt,omitempty"`
	// RestackBlockedFrom is set while a blocked task is blocked because a restack of its
	// branch conflicted: the status (verified or published) it had before, which a successful
	// retry of the restack restores. Any later status change clears it.
	RestackBlockedFrom string `json:"restack_blocked_from,omitempty"`
}

func (s *Store) ts() string { return s.now().UTC().Format(timeLayout) }

// CreateTask stores a new pending task and records a task-created event.
func (s *Store) CreateTask(ctx context.Context, spec taskspec.Spec) error {
	acc, _ := json.Marshal(spec.Acceptance)
	chk, _ := json.Marshal(spec.Checks)
	own, _ := json.Marshal(spec.Owns)
	now := s.ts()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id, title, kind, spec, acceptance, checks, owns, size, complexity, complexity_reason, model,
		status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.ID, spec.Title, spec.Kind, spec.Spec, string(acc), string(chk), string(own), spec.Size,
		spec.Complexity, spec.ComplexityReason, spec.Model, TaskPending, now, now)
	if err != nil {
		return fmt.Errorf("create task %s: %w", spec.ID, err)
	}
	_, err = s.AppendEvent(ctx, "task-created", "", spec.ID, map[string]string{"title": spec.Title})
	return err
}

// GetTask loads a task.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var goal, approvedAt sql.NullString
	var approvedAttempt sql.NullInt64
	var acc, chk, own, created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, goal_id, title, kind, spec, acceptance, checks, owns,
		size, complexity, complexity_reason, model, status, phase, reason, created_at, updated_at, approved_at,
		approved_attempt FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &goal, &t.Title, &t.Kind, &t.Spec.Spec, &acc, &chk, &own, &t.Size, &t.Complexity,
			&t.ComplexityReason, &t.Model, &t.Status, &t.Phase, &t.Reason, &created, &updated, &approvedAt,
			&approvedAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, err
	}
	t.GoalID = goal.String
	if t.Complexity == "" {
		// Tasks stored before complexity existed count as medium.
		t.Complexity = "medium"
	}
	t.Approved = approvedAt.Valid
	t.ApprovedAt = approvedAt.String
	t.ApprovedAttempt = int(approvedAttempt.Int64)
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
	if t.Status == TaskBlocked {
		// Every status change records a task-status event; the latest one tells whether the
		// block came from a restack.
		var data string
		err := s.db.QueryRowContext(ctx, `SELECT data FROM events WHERE task_id = ? AND kind = 'task-status'
			ORDER BY seq DESC LIMIT 1`, id).Scan(&data)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Task{}, fmt.Errorf("task %s: read its last status change: %w", id, err)
		}
		if err == nil {
			var d map[string]any
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return Task{}, fmt.Errorf("task %s: corrupt status event: %w", id, err)
			}
			if from, ok := d["restack_from"].(string); ok && d["status"] == TaskBlocked {
				t.RestackBlockedFrom = from
			}
		}
		if t.RestackBlockedFrom == "" && legacyRestackReason(t.Reason) {
			// Blocked by a restack conflict before blocks recorded the former status: a task
			// with a published commit was published, any other was verified.
			b, err := s.GetBranch(ctx, id)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return Task{}, fmt.Errorf("task %s: read its branch: %w", id, err)
			}
			t.RestackBlockedFrom = TaskVerified
			if b.PublishedSHA != "" {
				t.RestackBlockedFrom = TaskPublished
			}
		}
	}
	return t, nil
}

// legacyRestackReason reports whether reason is one sync gave when a restack conflicted, before
// such blocks recorded the task's former status. Only sync's prefix is unambiguous: the keeper
// gave the same reason for failures after a resolution (verifier), which must not be retried.
func legacyRestackReason(reason string) bool {
	return strings.HasPrefix(reason, "sync: restacking ")
}

// BlockRestack blocks a verified or published task whose restack conflicted, recording reason
// and the status it had (from), so that a later successful restack can restore it. The
// task's approval is kept.
func (s *Store) BlockRestack(ctx context.Context, id, from, reason string) error {
	if from != TaskVerified && from != TaskPublished {
		return fmt.Errorf("block %s after a restack: it was %s, but only a verified or published task is restacked", id, from)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("block task %s: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, phase = '', reason = ?, updated_at = ? WHERE id = ?`,
		TaskBlocked, reason, s.ts(), id)
	if err != nil {
		return fmt.Errorf("block task %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err := s.appendEventTx(ctx, tx, "task-status", "", id,
		map[string]string{"status": TaskBlocked, "phase": "", "reason": reason, "restack_from": from}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("block task %s: %w", id, err)
	}
	return nil
}

// SetTaskStatus changes a task's status, phase and reason and records a task-status event.
// Setting a task pending clears its approval in the same transaction.
func (s *Store) SetTaskStatus(ctx context.Context, id, status, phase, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set status of task %s: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, phase = ?, reason = ?, updated_at = ? WHERE id = ?`,
		status, phase, reason, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err := s.appendEventTx(ctx, tx, "task-status", "", id,
		map[string]string{"status": status, "phase": phase, "reason": reason}); err != nil {
		return err
	}
	if status == TaskPending {
		if err := s.clearApprovalTx(ctx, tx, "", id, "task set pending: "+reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ApproveTask records the operator's approval of a verified task's attempt and a
// task-approved event. A task in any other status is refused and left unchanged.
func (s *Store) ApproveTask(ctx context.Context, id string, attempt int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("approve task %s: %w", id, err)
	}
	defer tx.Rollback()
	var status, g string
	err = tx.QueryRowContext(ctx, `SELECT status, COALESCE(goal_id, '') FROM tasks WHERE id = ?`, id).Scan(&status, &g)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("approve task %s: %w", id, err)
	}
	switch status {
	case TaskVerified:
	case TaskPublished:
		return fmt.Errorf("task %s is published, not verified: published tasks are never queued, merge or reject its pull request instead", id)
	default:
		return fmt.Errorf("task %s is %s, not verified: only a verified task can be approved", id, status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET approved_at = ?, approved_attempt = ? WHERE id = ?`,
		s.ts(), attempt, id); err != nil {
		return fmt.Errorf("approve task %s: %w", id, err)
	}
	if err := s.appendEventTx(ctx, tx, "task-approved", g, id, map[string]int{"attempt": attempt}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("approve task %s: %w", id, err)
	}
	return nil
}

// ClearApproval clears a task's approval and records a task-approval-cleared event with
// reason. A task without an approval is left alone and no event is recorded.
func (s *Store) ClearApproval(ctx context.Context, id, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clear approval of %s: %w", id, err)
	}
	defer tx.Rollback()
	var g string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(goal_id, '') FROM tasks WHERE id = ?`, id).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("clear approval of %s: %w", id, err)
	}
	if err := s.clearApprovalTx(ctx, tx, g, id, reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("clear approval of %s: %w", id, err)
	}
	return nil
}

// clearApprovalTx clears a task's approval through tx and records task-approval-cleared, only
// when the task has an approval.
func (s *Store) clearApprovalTx(ctx context.Context, tx *sql.Tx, goalID, id, reason string) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET approved_at = NULL, approved_attempt = NULL
		WHERE id = ? AND approved_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("clear approval of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear approval of %s: %w", id, err)
	}
	if n == 0 {
		return nil
	}
	return s.appendEventTx(ctx, tx, "task-approval-cleared", goalID, id, map[string]string{"reason": reason})
}

// ApprovedTasks returns the approved tasks whose status is verified, dependencies before
// dependents (ties by created_at, then id).
func (s *Store) ApprovedTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE approved_at IS NOT NULL AND status = ?
		ORDER BY created_at, id`, TaskVerified)
	if err != nil {
		return nil, fmt.Errorf("list approved tasks: %w", err)
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

	// Order over all of task_deps (deps), so an approved task follows an approved ancestor
	// even when the task between them is not in the list.
	drows, err := s.db.QueryContext(ctx, `SELECT task_id, depends_on FROM deps`)
	if err != nil {
		return nil, fmt.Errorf("list approved tasks: %w", err)
	}
	parents := map[string][]string{}
	for drows.Next() {
		var c, p string
		if err := drows.Scan(&c, &p); err != nil {
			drows.Close()
			return nil, err
		}
		parents[c] = append(parents[c], p)
	}
	if err := drows.Err(); err != nil {
		drows.Close()
		return nil, err
	}
	drows.Close()

	// ancestors[id] is every task id depends on, directly or transitively.
	ancestors := map[string]map[string]bool{}
	for _, id := range ids {
		seen := map[string]bool{}
		queue := append([]string{}, parents[id]...)
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			if seen[p] {
				continue
			}
			seen[p] = true
			queue = append(queue, parents[p]...)
		}
		ancestors[id] = seen
	}
	// Repeatedly take the first task (in created_at, id order) that depends on none of the
	// remaining tasks. A dependency cycle cannot block the loop: it falls back to the first.
	var out []Task
	remaining := ids
	for len(remaining) > 0 {
		pick := 0
		for i, c := range remaining {
			ready := true
			for j, o := range remaining {
				if i != j && ancestors[c][o] {
					ready = false
					break
				}
			}
			if ready {
				pick = i
				break
			}
		}
		t, err := s.GetTask(ctx, remaining[pick])
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		remaining = append(append([]string{}, remaining[:pick]...), remaining[pick+1:]...)
	}
	return out, nil
}

// SetTaskReason changes a task's reason only; its status and phase are left as they are.
func (s *Store) SetTaskReason(ctx context.Context, id, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET reason = ?, updated_at = ? WHERE id = ?`, reason, s.ts(), id)
	if err != nil {
		return fmt.Errorf("set reason of task %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return nil
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
	PID        int          `json:"pid"`
	Command    string       `json:"command"`
	// CostEstimated is true when Usage.CostUSD was estimated from tokens.
	CostEstimated bool `json:"cost_estimated"`
	// Model is the model the attempt ran with and ModelReason why it was chosen.
	Model       string `json:"model,omitempty"`
	ModelReason string `json:"model_reason,omitempty"`
}

// Attempt statuses.
const (
	AttemptRunning   = "running"
	AttemptAbandoned = "abandoned"
)

// StartAttempt records the start of an agent run and returns its id.
// An implementer or fixer run gives the task new code, so it clears the task's approval in
// the same transaction; other roles (verifier, resolver) keep it.
func (s *Store) StartAttempt(ctx context.Context, taskID string, n int, role string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO attempts (task_id, n, role, status, started_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, n, role, AttemptRunning, s.ts())
	if err != nil {
		return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
	}
	if role == RoleImplementer || role == RoleFixer {
		var g string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(goal_id, '') FROM tasks WHERE id = ?`, taskID).Scan(&g); err != nil {
			return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
		}
		if err := s.clearApprovalTx(ctx, tx, g, taskID, fmt.Sprintf("%s attempt %d started", role, n)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("start attempt %s #%d %s: %w", taskID, n, role, err)
	}
	return id, nil
}

// Attempt roles that give a task new code; they match runner.RoleImplementer and
// runner.RoleFixer, which the pipeline passes to StartAttempt.
const (
	RoleImplementer = "implementer"
	RoleFixer       = "fixer"
)

// FinishAttempt records how an agent run ended and what it spent. costEstimated marks a cost
// estimated from tokens rather than reported by the agent.
func (s *Store) FinishAttempt(ctx context.Context, id int64, status string, u budget.Usage, costEstimated bool, outputPath, logPath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attempts SET status = ?, ended_at = ?, cost_estimated = ?,
		input_tokens = ?, output_tokens = ?, cache_write_tokens = ?, cache_read_tokens = ?, cost_usd = ?,
		output_path = ?, log_path = ? WHERE id = ?`,
		status, s.ts(), costEstimated, u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens, u.CostUSD,
		outputPath, logPath, id)
	return err
}

// UpdateAttemptUsage records the usage so far of a running attempt. It leaves a finished
// attempt untouched: FinishAttempt holds the final numbers.
func (s *Store) UpdateAttemptUsage(ctx context.Context, id int64, u budget.Usage, costEstimated bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attempts SET cost_estimated = ?, input_tokens = ?, output_tokens = ?,
		cache_write_tokens = ?, cache_read_tokens = ?, cost_usd = ? WHERE id = ? AND status = ?`,
		costEstimated, u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens, u.CostUSD, id, AttemptRunning)
	if err != nil {
		return fmt.Errorf("update usage of attempt %d: %w", id, err)
	}
	return nil
}

// SetAttemptProcess records the worker process (and its raw output file) of a running attempt.
func (s *Store) SetAttemptProcess(ctx context.Context, id int64, pid int, command, logPath string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE attempts SET pid = ?, command = ?, log_path = ? WHERE id = ? AND status = ?`,
		pid, command, logPath, id, AttemptRunning)
	if err != nil {
		return fmt.Errorf("record process of attempt %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("record process of attempt %d: no running attempt with that id", id)
	}
	return nil
}

// SetAttemptModel records the model an attempt runs with and why it was chosen.
func (s *Store) SetAttemptModel(ctx context.Context, attemptID int64, model, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE attempts SET model = ?, model_reason = ? WHERE id = ?`,
		model, reason, attemptID)
	if err != nil {
		return fmt.Errorf("record model of attempt %d: %w", attemptID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record model of attempt %d: %w", attemptID, err)
	}
	if n == 0 {
		return fmt.Errorf("record model of attempt %d: attempt %d: %w", attemptID, attemptID, ErrNotFound)
	}
	return nil
}

// RunningAttempts lists attempts whose status is running.
func (s *Store) RunningAttempts(ctx context.Context) ([]Attempt, error) {
	return s.queryAttempts(ctx, `WHERE status = ?`, AttemptRunning)
}

// RunningStage is a running task that has no running agent attempt: it is in a stage mill
// runs itself (prepare, gate, commit, checks, rebase, ...), or between two agent runs.
type RunningStage struct {
	TaskID string    `json:"task_id"`
	Phase  string    `json:"phase"` // as `mill goal show` prints it in "running (<phase>)"
	N      int       `json:"n"`     // highest attempt number used (0 if none)
	Since  time.Time `json:"since"` // when the task entered the phase
}

// RunningStages lists tasks whose status is running and that have no running attempt.
func (s *Store) RunningStages(ctx context.Context) ([]RunningStage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.phase, t.updated_at,
		COALESCE((SELECT MAX(a.n) FROM attempts a WHERE a.task_id = t.id), 0)
		FROM tasks t WHERE t.status = ?
		AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.task_id = t.id AND a.status = ?)
		ORDER BY t.updated_at, t.id`, TaskRunning, AttemptRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningStage
	for rows.Next() {
		var r RunningStage
		var since string
		if err := rows.Scan(&r.TaskID, &r.Phase, &since, &r.N); err != nil {
			return nil, err
		}
		r.Since, _ = parseTime(since)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Attempts lists a task's attempts in order.
func (s *Store) Attempts(ctx context.Context, taskID string) ([]Attempt, error) {
	return s.queryAttempts(ctx, `WHERE task_id = ?`, taskID)
}

func (s *Store) queryAttempts(ctx context.Context, where string, arg any) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, n, role, status, started_at, ended_at,
		input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost_usd, output_path, log_path,
		pid, command, cost_estimated, model, model_reason FROM attempts `+where+` ORDER BY id`, arg)
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
			&a.Usage.CostUSD, &a.OutputPath, &a.LogPath, &a.PID, &a.Command, &a.CostEstimated,
			&a.Model, &a.ModelReason); err != nil {
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
var TaskStatuses = []string{TaskPending, TaskRunning, TaskVerified, TaskPublished, TaskLanded, TaskBlocked, TaskFailed}

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
	StartSHA     string    `json:"start_sha,omitempty"`
	Worktree     string    `json:"worktree"`
	PublishedRef string    `json:"published_ref,omitempty"`
	PublishedSHA string    `json:"published_sha,omitempty"`
	GateSHA      string    `json:"gate_sha,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SaveBranch inserts or updates a task's branch row.
func (s *Store) SaveBranch(ctx context.Context, b Branch) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO branches
		(task_id, name, base, base_sha, start_sha, head_sha, worktree, published_ref, published_sha, gate_sha, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET name = excluded.name, base = excluded.base,
		base_sha = excluded.base_sha, start_sha = excluded.start_sha, head_sha = excluded.head_sha, worktree = excluded.worktree,
		published_ref = excluded.published_ref, published_sha = excluded.published_sha, gate_sha = excluded.gate_sha,
		updated_at = excluded.updated_at`,
		b.TaskID, b.Name, b.Base, b.BaseSHA, b.StartSHA, b.HeadSHA, b.Worktree, b.PublishedRef, b.PublishedSHA, b.GateSHA, s.ts())
	return err
}

// GetBranch loads a task's branch row.
func (s *Store) GetBranch(ctx context.Context, taskID string) (Branch, error) {
	var b Branch
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT task_id, name, base, base_sha, start_sha, head_sha, worktree,
		published_ref, published_sha, gate_sha, updated_at FROM branches WHERE task_id = ?`, taskID).
		Scan(&b.TaskID, &b.Name, &b.Base, &b.BaseSHA, &b.StartSHA, &b.HeadSHA, &b.Worktree, &b.PublishedRef,
			&b.PublishedSHA, &b.GateSHA, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Branch{}, fmt.Errorf("branch for %s: %w", taskID, ErrNotFound)
	}
	b.UpdatedAt, _ = parseTime(updated)
	return b, err
}

// MarkTaskCleaned clears a task's worktree path (the branch row and its history stay) and
// records a task-cleaned event.
func (s *Store) MarkTaskCleaned(ctx context.Context, taskID string, data map[string]string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE branches SET worktree = '', updated_at = ? WHERE task_id = ?`, s.ts(), taskID)
	if err != nil {
		return fmt.Errorf("clear worktree of %s: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("branch for %s: %w", taskID, ErrNotFound)
	}
	_, err = s.AppendEvent(ctx, "task-cleaned", "", taskID, data)
	return err
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
		checks = ?, owns = ?, size = ?, complexity = ?, complexity_reason = ?, model = ?, updated_at = ? WHERE id = ?`,
		spec.Title, spec.Kind, spec.Spec, string(acc), string(chk), string(own), spec.Size,
		spec.Complexity, spec.ComplexityReason, spec.Model, s.ts(), spec.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s: %w", spec.ID, ErrNotFound)
	}
	_, err = s.AppendEvent(ctx, "task-spec-updated", "", spec.ID, nil)
	return err
}

// AbandonedUsage is the usage recovered for an attempt abandoned by RecoverInterrupted.
type AbandonedUsage struct {
	Usage         budget.Usage
	CostEstimated bool
}

// RecoverInterrupted repairs the state a killed run left behind: running tasks go back to
// pending and running attempts become abandoned, in one transaction, recording the usage
// given per attempt id (attempts not in usage keep zero), and task claims of dead
// processes are deleted. It records a task-recovered event
// per task. Only call it while holding the run lock.
func (s *Store) RecoverInterrupted(ctx context.Context, usage map[int64]AbandonedUsage) (tasks int, attempts int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("recover interrupted run: %w", err)
	}
	defer tx.Rollback()
	const reason = "recovered after an interrupted run"

	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE status = ? ORDER BY id`, TaskRunning)
	if err != nil {
		return 0, 0, fmt.Errorf("recover interrupted run: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	now := s.ts()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, phase = '', reason = ?, updated_at = ? WHERE id = ?`,
			TaskPending, reason, now, id); err != nil {
			return 0, 0, fmt.Errorf("recover task %s: %w", id, err)
		}
		payload, _ := json.Marshal(map[string]string{"status": TaskPending, "reason": reason})
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (at, kind, goal_id, task_id, data) VALUES (?, ?, NULL, ?, ?)`,
			now, "task-recovered", id, string(payload)); err != nil {
			return 0, 0, fmt.Errorf("record recovery of %s: %w", id, err)
		}
		if err := s.clearApprovalTx(ctx, tx, "", id, reason); err != nil {
			return 0, 0, err
		}
	}
	arows, err := tx.QueryContext(ctx, `SELECT id FROM attempts WHERE status = ? ORDER BY id`, AttemptRunning)
	if err != nil {
		return 0, 0, fmt.Errorf("recover attempts: %w", err)
	}
	var aids []int64
	for arows.Next() {
		var id int64
		if err := arows.Scan(&id); err != nil {
			arows.Close()
			return 0, 0, err
		}
		aids = append(aids, id)
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		return 0, 0, err
	}
	arows.Close()
	for _, id := range aids {
		u := usage[id]
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = ?, ended_at = ?, cost_estimated = ?,
			input_tokens = ?, output_tokens = ?, cache_write_tokens = ?, cache_read_tokens = ?, cost_usd = ?
			WHERE id = ?`, AttemptAbandoned, now, u.CostEstimated, u.Usage.InputTokens, u.Usage.OutputTokens,
			u.Usage.CacheWriteTokens, u.Usage.CacheReadTokens, u.Usage.CostUSD, id); err != nil {
			return 0, 0, fmt.Errorf("recover attempt %d: %w", id, err)
		}
	}
	n := len(aids)
	if err := s.deleteDeadClaimsTx(ctx, tx); err != nil {
		return 0, 0, fmt.Errorf("recover interrupted run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("recover interrupted run: %w", err)
	}
	return len(ids), n, nil
}
