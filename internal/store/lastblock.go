package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// LastBlockedPhase returns the phase and reason of the task's most recent task-status event
// whose status is blocked. Both are empty when the task was never blocked, or when a later
// event moved it to verified, published or landed. A later reset to pending keeps the result,
// since that is what --retry-blocked does.
func (s *Store) LastBlockedPhase(ctx context.Context, taskID string) (phase, reason string, err error) {
	evs, err := s.queryEvents(ctx, `SELECT seq, at, kind, COALESCE(goal_id, ''), COALESCE(task_id, ''), data
		FROM events WHERE task_id = ? AND kind = 'task-status' ORDER BY seq DESC`, taskID)
	if err != nil {
		return "", "", fmt.Errorf("task %s: read its status changes: %w", taskID, err)
	}
	for _, e := range evs {
		var d struct {
			Status string `json:"status"`
			Phase  string `json:"phase"`
			Reason string `json:"reason"`
		}
		err := json.Unmarshal(e.Data, &d)
		if err == nil && d.Status == "" {
			err = errors.New("no status")
		}
		if err != nil {
			return "", "", fmt.Errorf("task %s: corrupt status event %d: %w", taskID, e.Seq, err)
		}
		if d.Status == TaskBlocked {
			return d.Phase, d.Reason, nil
		}
		if Done(d.Status) {
			break
		}
	}
	return "", "", nil
}
