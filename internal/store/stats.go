package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/msidorkevich/mill/internal/taskspec"
)

// ModelStat summarises the tasks of one (size, complexity, model) group. Model is the model of
// the task's first implementer attempt, "-" when it was not recorded.
type ModelStat struct {
	Size       string `json:"size"`
	Complexity string `json:"complexity"`
	Model      string `json:"model"`
	Tasks      int    `json:"tasks"`
	// FirstAttemptPass is the share (0..1) of tasks that are verified, published or landed
	// and whose only implementer attempt is attempt 1.
	FirstAttemptPass float64 `json:"first_attempt_pass"`
	// AvgAttempts is the mean number of implementer attempts per task.
	AvgAttempts float64 `json:"avg_attempts"`
	// AvgCostUSD is the mean of each task's total cost over all its attempts.
	AvgCostUSD float64 `json:"avg_cost_usd"`
}

// complexityRank orders complexities low, medium, high; unknown values sort after them.
var complexityRank = map[string]int{"low": 0, "medium": 1, "high": 2}

// ModelStats groups the tasks that have at least one implementer attempt by size,
// complexity and first-attempt model, ordered by size, complexity (low, medium, high) and
// model. Sizes follow taskspec.Sizes (S, M). It changes nothing.
func (s *Store) ModelStats(ctx context.Context) ([]ModelStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.size, t.complexity, t.status,
		COALESCE((SELECT a.model FROM attempts a WHERE a.task_id = t.id AND a.role = ?
			ORDER BY a.n, a.id LIMIT 1), ''),
		(SELECT COUNT(*) FROM attempts a WHERE a.task_id = t.id AND a.role = ?),
		(SELECT MIN(a.n) FROM attempts a WHERE a.task_id = t.id AND a.role = ?),
		(SELECT COALESCE(SUM(a.cost_usd), 0) FROM attempts a WHERE a.task_id = t.id)
		FROM tasks t
		WHERE EXISTS (SELECT 1 FROM attempts a WHERE a.task_id = t.id AND a.role = ?)`,
		RoleImplementer, RoleImplementer, RoleImplementer, RoleImplementer)
	if err != nil {
		return nil, fmt.Errorf("read model stats: %w", err)
	}
	defer rows.Close()

	type key struct{ size, complexity, model string }
	type acc struct {
		tasks, passed, attempts int
		cost                    float64
	}
	groups := map[key]*acc{}
	for rows.Next() {
		var size, complexity, status, model string
		var attempts, firstN int
		var cost float64
		if err := rows.Scan(&size, &complexity, &status, &model, &attempts, &firstN, &cost); err != nil {
			return nil, fmt.Errorf("read model stats: %w", err)
		}
		if complexity == "" {
			complexity = "medium" // as GetTask reads tasks created before complexity was stored
		}
		if model == "" {
			model = "-"
		}
		k := key{size, complexity, model}
		g := groups[k]
		if g == nil {
			g = &acc{}
			groups[k] = g
		}
		g.tasks++
		g.attempts += attempts
		g.cost += cost
		switch status {
		case TaskVerified, TaskPublished, TaskLanded:
			if attempts == 1 && firstN == 1 {
				g.passed++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model stats: %w", err)
	}

	out := make([]ModelStat, 0, len(groups))
	for k, g := range groups {
		n := float64(g.tasks)
		out = append(out, ModelStat{
			Size: k.size, Complexity: k.complexity, Model: k.model, Tasks: g.tasks,
			FirstAttemptPass: float64(g.passed) / n,
			AvgAttempts:      float64(g.attempts) / n,
			AvgCostUSD:       g.cost / n,
		})
	}
	rank := func(c string) int {
		if r, ok := complexityRank[c]; ok {
			return r
		}
		return len(complexityRank)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Size != b.Size {
			// Sizes in taskspec order (S, M); unknown sizes after them, alphabetically.
			sa, sb := slices.Index(taskspec.Sizes, a.Size), slices.Index(taskspec.Sizes, b.Size)
			if sa < 0 {
				sa = len(taskspec.Sizes)
			}
			if sb < 0 {
				sb = len(taskspec.Sizes)
			}
			if sa != sb {
				return sa < sb
			}
			return a.Size < b.Size
		}
		if ra, rb := rank(a.Complexity), rank(b.Complexity); ra != rb {
			return ra < rb
		}
		if a.Complexity != b.Complexity {
			return a.Complexity < b.Complexity
		}
		return a.Model < b.Model
	})
	return out, nil
}

// GroupTaskCosts returns the total cost (all attempts, retries included) of each finished
// task (verified, published, landed, failed or blocked) with at least one implementer attempt
// in the (size, complexity, model) group, grouped as ModelStats groups them: model is the
// model of the task's first implementer attempt ("-" when unrecorded) and a task without a
// stored complexity counts as medium. Costs are in ascending order. It changes nothing.
func (s *Store) GroupTaskCosts(ctx context.Context, size, complexity, model string) ([]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.complexity,
		COALESCE((SELECT a.model FROM attempts a WHERE a.task_id = t.id AND a.role = ?
			ORDER BY a.n, a.id LIMIT 1), ''),
		(SELECT COALESCE(SUM(a.cost_usd), 0) FROM attempts a WHERE a.task_id = t.id)
		FROM tasks t
		WHERE t.size = ? AND t.status IN (?, ?, ?, ?, ?)
		AND EXISTS (SELECT 1 FROM attempts a WHERE a.task_id = t.id AND a.role = ?)`,
		RoleImplementer, size, TaskVerified, TaskPublished, TaskLanded, TaskFailed, TaskBlocked,
		RoleImplementer)
	if err != nil {
		return nil, fmt.Errorf("read task costs of %s/%s/%s: %w", size, complexity, model, err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var c, m string
		var cost float64
		if err := rows.Scan(&c, &m, &cost); err != nil {
			return nil, fmt.Errorf("read task costs of %s/%s/%s: %w", size, complexity, model, err)
		}
		if c == "" {
			c = "medium" // as ModelStats
		}
		if m == "" {
			m = "-"
		}
		if c == complexity && m == model {
			out = append(out, cost)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read task costs of %s/%s/%s: %w", size, complexity, model, err)
	}
	sort.Float64s(out)
	return out, nil
}

// AttemptDurations returns how long, in milliseconds, each finished implementer attempt
// ran on model for a task of the given size and complexity (a task without a stored
// complexity counts as medium, as ModelStats). An attempt's duration is its agent-reported
// agent_ms when present, else its wall time from start to end. Durations are in ascending
// order. It changes nothing.
func (s *Store) AttemptDurations(ctx context.Context, size, complexity, model string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.complexity, a.started_at, a.ended_at, a.agent_ms
		FROM attempts a JOIN tasks t ON t.id = a.task_id
		WHERE a.role = ? AND a.model = ? AND t.size = ? AND a.ended_at IS NOT NULL
		AND a.status NOT IN (?, ?)`,
		RoleImplementer, model, size, AttemptRunning, AttemptAbandoned)
	if err != nil {
		return nil, fmt.Errorf("read attempt durations of %s/%s/%s: %w", size, complexity, model, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var c, started, ended string
		var agentMS sql.NullInt64
		if err := rows.Scan(&c, &started, &ended, &agentMS); err != nil {
			return nil, fmt.Errorf("read attempt durations of %s/%s/%s: %w", size, complexity, model, err)
		}
		if c == "" {
			c = "medium" // as ModelStats
		}
		if c != complexity {
			continue
		}
		if agentMS.Valid && agentMS.Int64 > 0 {
			out = append(out, agentMS.Int64)
			continue
		}
		st, err := parseTime(started)
		if err != nil {
			return nil, fmt.Errorf("read attempt durations of %s/%s/%s: bad start time %q: %w", size, complexity, model, started, err)
		}
		en, err := parseTime(ended)
		if err != nil {
			return nil, fmt.Errorf("read attempt durations of %s/%s/%s: bad end time %q: %w", size, complexity, model, ended, err)
		}
		out = append(out, en.Sub(st).Milliseconds())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read attempt durations of %s/%s/%s: %w", size, complexity, model, err)
	}
	slices.Sort(out)
	return out, nil
}

// GateDurations returns how long, in milliseconds, each gate the pipeline ran took: from the
// task-status event that put the task in phase gate to the task's next task-gate event.
// Reused gates (which run no command) and gates without a recorded start are left out.
// Durations are in ascending order. It changes nothing.
func (s *Store) GateDurations(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, at, kind, data FROM events
		WHERE task_id IS NOT NULL AND kind IN ('task-status', 'task-gate') ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("read gate durations: %w", err)
	}
	defer rows.Close()
	started := map[string]time.Time{} // task id → when its running gate began
	var out []int64
	for rows.Next() {
		var task, at, kind, data string
		if err := rows.Scan(&task, &at, &kind, &data); err != nil {
			return nil, fmt.Errorf("read gate durations: %w", err)
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, fmt.Errorf("read gate durations: bad event time %q: %w", at, err)
		}
		if kind == "task-status" {
			var d struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return nil, fmt.Errorf("read gate durations: corrupt task-status event of %s: %w", task, err)
			}
			if d.Phase == PhaseGate {
				started[task] = t
			} else {
				delete(started, task)
			}
			continue
		}
		var d struct {
			Reused bool `json:"reused"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return nil, fmt.Errorf("read gate durations: corrupt task-gate event of %s: %w", task, err)
		}
		if st, ok := started[task]; ok && !d.Reused {
			out = append(out, t.Sub(st).Milliseconds())
		}
		delete(started, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read gate durations: %w", err)
	}
	slices.Sort(out)
	return out, nil
}
