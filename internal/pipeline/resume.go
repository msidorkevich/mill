package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// resume runs the verify step of attempt n (the task's latest) when the task was blocked in
// verify after its gate passed and nothing changed since: no implementer and no gate run.
// resumed is false when the run must start a new implementer attempt as usual. Otherwise done
// means the run ends with out or err, and findings are the verifier's for the next attempt.
func (p *Pipeline) resume(ctx context.Context, t store.Task, br store.Branch, startSHA string, n int, limits budget.Limits, out Outcome) (_ Outcome, findings []prompt.Finding, resumed, done bool, err error) {
	g, ok, err := p.resumeGate(ctx, t, br, n)
	if err != nil || !ok {
		return out, nil, false, false, err
	}
	p.logf("%s a%d resuming at the verifier (gate passed at %s)", t.ID, n, short(br.GateSHA))
	outside, err := p.outsideOwns(ctx, t, br.Worktree, startSHA, n)
	if err != nil {
		return out, nil, true, true, err
	}
	implModel, why, err := p.attemptImplementerModel(ctx, t, n)
	if err != nil {
		return out, nil, true, true, err
	}
	if implModel == "" {
		out, err = p.block(ctx, t.ID, store.PhaseVerify, why, out, nil)
		return out, nil, true, true, err
	}
	logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n))
	out, findings, done, err = p.verify(ctx, t, verifyStep{
		n: n, br: br, startSHA: startSHA, logDir: logDir, gate: g, outside: outside,
		implModel: implModel, limits: limits,
	}, out)
	return out, findings, true, done, err
}

// resumeGate decides whether attempt n of t resumes at the verifier, and returns the passed
// gate result recorded for that attempt at the branch head. It resumes only when the task
// was last blocked in verify, the gate passed on the recorded head, the worktree is at that
// head with no uncommitted changes, and the passed gate is in the event log.
func (p *Pipeline) resumeGate(ctx context.Context, t store.Task, br store.Branch, n int) (gate.Result, bool, error) {
	if n <= 0 || br.GateSHA == "" || br.GateSHA != br.HeadSHA {
		return gate.Result{}, false, nil
	}
	phase, _, err := p.Store.LastBlockedPhase(ctx, t.ID)
	if err != nil || phase != store.PhaseVerify {
		return gate.Result{}, false, err
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return gate.Result{}, false, fmt.Errorf("read the head of %s in %s to decide whether to resume: %w", br.Name, br.Worktree, err)
	}
	if head != br.HeadSHA {
		return gate.Result{}, false, nil
	}
	changed, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return gate.Result{}, false, fmt.Errorf("check %s for uncommitted changes to decide whether to resume: %w", br.Worktree, err)
	}
	if changed {
		return gate.Result{}, false, nil
	}
	return p.recordedGate(ctx, t.ID, n, head)
}

// recordedGate returns the result of the last task-gate event of attempt n at head, ok only
// when that gate passed.
func (p *Pipeline) recordedGate(ctx context.Context, taskID string, n int, head string) (gate.Result, bool, error) {
	evs, err := p.Store.TaskEvents(ctx, taskID)
	if err != nil {
		return gate.Result{}, false, fmt.Errorf("read the events of %s: %w", taskID, err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "task-gate" {
			continue
		}
		var d struct {
			Attempt int             `json:"attempt"`
			Head    string          `json:"head"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil {
			return gate.Result{}, false, fmt.Errorf("read the gate event %d of %s: %w", evs[i].Seq, taskID, err)
		}
		if d.Attempt != n || d.Head != head {
			continue
		}
		var g gate.Result
		if err := json.Unmarshal(d.Result, &g); err != nil {
			return gate.Result{}, false, fmt.Errorf("read the gate result in event %d of %s: %w", evs[i].Seq, taskID, err)
		}
		return g, g.Passed, nil
	}
	return gate.Result{}, false, nil
}

// attemptImplementerModel is the model recorded for the implementer (or fixer) of attempt n,
// or, when none was recorded, the model the implementer of attempt n would run on.
func (p *Pipeline) attemptImplementerModel(ctx context.Context, t store.Task, n int) (model, why string, err error) {
	attempts, err := p.Store.Attempts(ctx, t.ID)
	if err != nil {
		return "", "", fmt.Errorf("read the attempts of %s: %w", t.ID, err)
	}
	for _, a := range attempts {
		if a.N == n && (a.Role == store.RoleImplementer || a.Role == store.RoleFixer) && a.Model != "" {
			model = a.Model
		}
	}
	if model != "" {
		return model, "", nil
	}
	return p.implementerModel(ctx, t, n)
}
