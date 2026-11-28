package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
)

// AgentOutcome is how one resolver run ended. Exactly one of Stop, Reason or a usable
// Output describes it: Stop means the task must stop (budget, rate limit, interruption,
// timeout); Reason means the run failed or its answer was unusable.
type AgentOutcome struct {
	Attempt int                   `json:"attempt"`
	Status  string                `json:"status"` // the runner status
	Output  prompt.ResolverOutput `json:"output"`
	Reason  string                `json:"reason,omitempty"`
	Stop    string                `json:"stop,omitempty"`
	// RateLimited is true when the run hit the account's usage limit; ResetsAt is when it
	// resets, when known.
	RateLimited bool         `json:"rate_limited,omitempty"`
	ResetsAt    time.Time    `json:"resets_at,omitempty"`
	Usage       budget.Usage `json:"usage"` // this run's usage
}

// Resolve runs the resolver agent in dir (a worktree stopped on conflicts) as the task's
// next attempt. It does not touch git: the caller stages the files and continues. A returned
// error means mill itself failed; a failed run is reported in the outcome.
func (p *Pipeline) Resolve(ctx context.Context, taskID string, in prompt.ResolverInput, dir string) (AgentOutcome, error) {
	t, err := p.Store.GetTask(ctx, taskID)
	if err != nil {
		return AgentOutcome{}, err
	}
	limits := p.Cfg.AttemptBudget(t.Size)
	if blocked, reason, err := p.checkBudgets(ctx, t, stepImplement); err != nil || blocked {
		return AgentOutcome{Stop: reason}, err
	}
	prev, err := p.Store.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return AgentOutcome{}, err
	}
	n := prev + 1
	model, why := p.Cfg.TaskModel(t.Size, t.Complexity, t.Model)
	if model == "" {
		return AgentOutcome{Attempt: n, Stop: why}, nil
	}
	logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n))
	in.CheckCmd, in.RunsCheck = p.Cfg.CheckCmd, p.Cfg.ImplementerRunsCheck
	ar, err := p.agent(ctx, t, n, runner.RoleResolver, model, why, prompt.Resolver(in), prompt.ResolverSchema, ImplementerTools, limits, dir, logDir)
	out := AgentOutcome{Attempt: n, Status: string(ar.res.Status), Usage: ar.res.Usage,
		Stop: ar.stop, RateLimited: ar.rateLimited, ResetsAt: ar.res.ResetsAt}
	if err != nil || out.Stop != "" {
		return out, err
	}
	if ar.res.Status != runner.StatusOK {
		out.Reason = "the resolver run failed: " + ar.res.Error
		return out, nil
	}
	res, perr := prompt.ParseResolver(ar.res.Output)
	if perr != nil {
		out.Reason = "the resolver's answer was unusable: " + perr.Error()
		return out, nil
	}
	out.Output = res
	return out, nil
}

// Verify runs the verifier on the task branch's current head against its starting point,
// as the task's next attempt. It records a task-verdict event and leaves the task status
// alone. A non-empty stop means the run had to stop (budget, rate limit, interruption,
// timeout) and verdict is empty.
func (p *Pipeline) Verify(ctx context.Context, taskID string) (prompt.VerifierOutput, string, error) {
	t, err := p.Store.GetTask(ctx, taskID)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	br, err := p.Store.GetBranch(ctx, t.ID)
	if err != nil {
		return prompt.VerifierOutput{}, "", fmt.Errorf("task %s has no branch to verify: %w", t.ID, err)
	}
	startSHA := br.StartSHA
	if startSHA == "" {
		startSHA = br.BaseSHA
	}
	limits := p.Cfg.AttemptBudget(t.Size)
	if blocked, reason, err := p.checkBudgets(ctx, t, stepVerify); err != nil || blocked {
		return prompt.VerifierOutput{}, reason, err
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return prompt.VerifierOutput{}, "", fmt.Errorf("read the head of %s in %s: %w", br.Name, br.Worktree, err)
	}
	prev, err := p.Store.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	n := prev + 1
	impl, err := p.latestImplementerModel(ctx, t.ID)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	if impl == "" {
		// No implementer attempt recorded a model: use what the implementer would run on.
		var why string
		if impl, why, err = p.implementerModel(ctx, t, n); err != nil {
			return prompt.VerifierOutput{}, "", err
		}
		if impl == "" {
			return prompt.VerifierOutput{}, why, nil
		}
	}
	verModel, verWhy := p.verifierModel(impl)
	logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n))

	diff, err := gitx.Diff(ctx, br.Worktree, startSHA)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	stat, err := gitx.DiffStat(ctx, br.Worktree, startSHA)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	if err := writeFile(filepath.Join(logDir, "diff.patch"), diff); err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	var outside []string
	if p.Cfg.OwnsPolicy != "off" {
		files, err := gitx.ChangedFiles(ctx, br.Worktree, startSHA)
		if err != nil {
			return prompt.VerifierOutput{}, "", fmt.Errorf("list changed files of %s: %w", br.Name, err)
		}
		outside = plan.OutsideOwns(t.Owns, files)
	}
	g := gate.Result{Reason: "mill runs the gate separately on this head; review the change itself"}
	sibs, err := p.siblings(ctx, t)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	// The head being verified may be newer than the recorded one.
	br.HeadSHA = head
	bc, err := p.baseChanges(ctx, t, br, n)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	requests, err := ScopeRequests(ctx, p.Store, t.ID)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	verPrompt := prompt.Verifier(prompt.VerifierInput{Task: t.Spec, BaseSHA: startSHA, Diff: diff, DiffStat: stat, Gate: g, OutsideOwns: outside, Siblings: sibs, BaseChanged: bc.Files, BaseChangedStat: bc.Stat, Requests: requests})
	vr, err := p.agent(ctx, t, n, runner.RoleVerifier, verModel, verWhy, verPrompt, prompt.VerifierSchema, VerifierTools, limits, br.Worktree, logDir)
	if err != nil {
		return prompt.VerifierOutput{}, "", err
	}
	if vr.stop != "" {
		return prompt.VerifierOutput{}, vr.stop, nil
	}
	if vr.res.Status != runner.StatusOK {
		return prompt.VerifierOutput{}, "", fmt.Errorf("the verifier could not complete its review of %s: %s; run the verification again", t.ID, vr.res.Error)
	}
	verdict, perr := prompt.ParseVerifier(vr.res.Output)
	if perr != nil {
		return prompt.VerifierOutput{}, "", fmt.Errorf("the verifier's answer for %s was unusable: %w; run the verification again", t.ID, perr)
	}
	if _, err := p.Store.AppendEvent(ctx, "task-verdict", "", t.ID, map[string]any{"attempt": n, "head": head, "verdict": verdict}); err != nil {
		return verdict, "", err
	}
	p.logf("%s a%d verdict   %s: %s", t.ID, n, verdict.Verdict, oneLine(verdict.Summary))
	return verdict, "", nil
}

// RecordGateFailure records a failed gate's commands as the findings of a new attempt
// number, so the next Run starts its implementer with them.
func (p *Pipeline) RecordGateFailure(ctx context.Context, taskID string, g gate.Result) error {
	prev, err := p.Store.MaxAttemptN(ctx, taskID)
	if err != nil {
		return err
	}
	findings := gateFindings(g)
	if len(findings) == 0 {
		return fmt.Errorf("gate result for %s has no failed command to record", taskID)
	}
	return p.recordFindings(ctx, taskID, prev+1, findings)
}
