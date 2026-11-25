package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GateResult is a passed gate stored for reuse: the same tree gated with the same
// check_cmd and task checks passes again without running.
type GateResult struct {
	TreeSHA     string          `json:"tree_sha"`
	Fingerprint string          `json:"fingerprint"`
	TaskID      string          `json:"task_id"` // the task whose gate passed
	Result      json.RawMessage `json:"result"`  // the passing gate result
	PassedAt    time.Time       `json:"passed_at"`
}

// GateFingerprint identifies the commands of a gate: check_cmd and the task's checks,
// sorted. Blank checks are ignored, as the gate skips them.
func GateFingerprint(checkCmd string, checks []string) string {
	var cs []string
	for _, c := range checks {
		if strings.TrimSpace(c) != "" {
			cs = append(cs, c)
		}
	}
	sort.Strings(cs)
	h := sha256.New()
	h.Write([]byte(checkCmd))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(cs, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}

// RecordGatePass stores a passing gate result for tree and fingerprint, replacing an
// earlier one. Only passing gates may be recorded.
func (s *Store) RecordGatePass(ctx context.Context, tree, fingerprint, taskID string, result json.RawMessage) error {
	if tree == "" || fingerprint == "" {
		return fmt.Errorf("record gate pass of %s: tree and fingerprint are required", taskID)
	}
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO gate_results (tree_sha, fingerprint, task_id, result, passed_at) VALUES (?, ?, ?, ?, ?)`,
		tree, fingerprint, taskID, string(result), s.ts()); err != nil {
		return fmt.Errorf("record gate pass of %s at tree %s: %w", taskID, tree, err)
	}
	return nil
}

// PassedGate returns the passing gate result stored for tree and fingerprint; ok is false
// when there is none.
func (s *Store) PassedGate(ctx context.Context, tree, fingerprint string) (r GateResult, ok bool, err error) {
	var res, at string
	err = s.db.QueryRowContext(ctx, `SELECT tree_sha, fingerprint, task_id, result, passed_at FROM gate_results WHERE tree_sha = ? AND fingerprint = ?`,
		tree, fingerprint).Scan(&r.TreeSHA, &r.Fingerprint, &r.TaskID, &res, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return GateResult{}, false, nil
	}
	if err != nil {
		return GateResult{}, false, fmt.Errorf("look up passed gate at tree %s: %w", tree, err)
	}
	r.Result = json.RawMessage(res)
	if r.PassedAt, err = parseTime(at); err != nil {
		return GateResult{}, false, fmt.Errorf("passed gate at tree %s has an unreadable time %q: %w", tree, at, err)
	}
	return r, true, nil
}
