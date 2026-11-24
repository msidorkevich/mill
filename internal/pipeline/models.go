package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/msidorkevich/mill/internal/store"
)

// implementerModel picks the model for implementer (or fixer) attempt n of t: the task's base
// model, escalated one tier for every earlier implementer attempt whose findings were
// recorded. An empty model means the config has no mapping; why then says what to add.
func (p *Pipeline) implementerModel(ctx context.Context, t store.Task, n int) (model, why string, err error) {
	base, baseWhy := p.Cfg.TaskModel(t.Size, t.Complexity, t.Model)
	if base == "" {
		return "", baseWhy, nil
	}
	failed, err := p.failedImplementerAttempts(ctx, t.ID, n)
	if err != nil {
		return "", "", err
	}
	model = p.Cfg.Escalate(base, len(failed))
	if model != base {
		return model, fmt.Sprintf("escalated after attempt %d failed", failed[len(failed)-1]), nil
	}
	return base, baseWhy, nil
}

// failedImplementerAttempts lists, in ascending order, the numbers of the implementer and
// fixer attempts of a task before attempt n that have findings recorded.
func (p *Pipeline) failedImplementerAttempts(ctx context.Context, taskID string, n int) ([]int, error) {
	attempts, err := p.Store.Attempts(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("read the attempts of %s: %w", taskID, err)
	}
	evs, err := p.Store.TaskEvents(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("read the events of %s: %w", taskID, err)
	}
	withFindings := map[int]bool{}
	for _, ev := range evs {
		if ev.Kind != "attempt-findings" {
			continue
		}
		var d struct {
			Attempt int `json:"attempt"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return nil, fmt.Errorf("read the findings event %d of %s: %w", ev.Seq, taskID, err)
		}
		withFindings[d.Attempt] = true
	}
	var out []int
	seen := map[int]bool{}
	for _, a := range attempts {
		if a.N >= n || seen[a.N] || !withFindings[a.N] {
			continue
		}
		if a.Role != store.RoleImplementer && a.Role != store.RoleFixer {
			continue
		}
		seen[a.N] = true
		out = append(out, a.N)
	}
	slices.Sort(out)
	return out, nil
}

// verifierModel picks the verifier's model for an implementer that ran on impl.
func (p *Pipeline) verifierModel(impl string) (model, why string) {
	model = p.Cfg.VerifierModel(impl)
	if model != impl {
		return model, "verifier floor " + p.Cfg.Roles.VerifierMin
	}
	return model, "implementer tier"
}

// latestImplementerModel returns the model recorded for the task's latest implementer or fixer
// attempt, or "" when none has one.
func (p *Pipeline) latestImplementerModel(ctx context.Context, taskID string) (string, error) {
	attempts, err := p.Store.Attempts(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("read the attempts of %s: %w", taskID, err)
	}
	model, latest := "", -1
	for _, a := range attempts {
		if (a.Role == store.RoleImplementer || a.Role == store.RoleFixer) && a.Model != "" && a.N >= latest {
			model, latest = a.Model, a.N
		}
	}
	return model, nil
}
