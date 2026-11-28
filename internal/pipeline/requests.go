package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// requestSources are the finding sources that ask for work after planning: they widen the
// task's scope rather than report a defect of one attempt.
var requestSources = []string{"operator", "merge-check", "ci"}

// ScopeRequests returns the operator, merge-check and ci findings recorded for a task since
// its last task-landed event (all of them when it never landed), oldest first, with exact
// duplicates collapsed.
func ScopeRequests(ctx context.Context, s *store.Store, taskID string) ([]prompt.Finding, error) {
	evs, err := s.TaskEvents(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("read the events of task %s to collect its requests: %w", taskID, err)
	}
	start := 0
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "task-landed" {
			start = i + 1
			break
		}
	}
	var out []prompt.Finding
	for _, ev := range evs[start:] {
		if ev.Kind != "attempt-findings" {
			continue
		}
		var d struct {
			Findings []prompt.Finding `json:"findings"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return nil, fmt.Errorf("read the findings of task %s in event %d: %w", taskID, ev.Seq, err)
		}
		for _, f := range d.Findings {
			if slices.Contains(requestSources, f.Source) && !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out, nil
}
