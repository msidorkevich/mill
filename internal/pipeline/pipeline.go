// Package pipeline runs one task: prepare a worktree, then implement → commit → gate →
// verify, retrying with findings until the task is verified, blocked or failed (TP1–TP6).
//
// Every state change goes through the store; agents never change task state themselves (P1).
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/fanin"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/price"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// Tools each role may use.
var (
	ImplementerTools = []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep"}
	VerifierTools    = []string{"Read", "Glob", "Grep"}
)

// Pipeline runs tasks for one repository.
type Pipeline struct {
	Store  *store.Store
	Runner runner.Runner
	Host   host.Host
	Cfg    config.Config
	Paths  project.Paths
	Home   string    // user home, for the default worktree root
	Out    io.Writer // progress lines; nil discards
	// AgentEnvAllow are extra variables agents receive (e.g. runner.ClaudeEnvAllow).
	AgentEnvAllow []string
	// Now is the clock for durations in progress lines; defaults to time.Now.
	Now func() time.Time

	// A Pipeline may run several tasks from different goroutines. gitMu serializes the git
	// operations that touch the shared repository; outMu keeps progress lines whole.
	gitMu sync.Mutex
	outMu sync.Mutex
}

// Outcome is how a run ended.
type Outcome struct {
	Status   string       `json:"status"` // verified, blocked, failed
	Reason   string       `json:"reason,omitempty"`
	HeadSHA  string       `json:"head_sha,omitempty"`
	Attempts int          `json:"attempts"` // implement attempts made in this run
	Usage    budget.Usage `json:"usage"`    // total task usage, all runs
	// RateLimited is true when an agent run hit the account's usage limit; the task is blocked.
	RateLimited bool `json:"rate_limited,omitempty"`
	// ResetsAt is when the usage limit resets, when RateLimited and known.
	ResetsAt time.Time `json:"resets_at,omitempty"`
}

// BranchName is the git branch for a task: mill/<goal>/<task> for a goal's task,
// mill/t/<id> for a single task.
func BranchName(taskID string) string {
	if goal, _ := store.SplitTaskID(taskID); goal != "" {
		return "mill/" + taskID
	}
	return "mill/t/" + taskID
}

// blockedError is returned by prepare when the task cannot start for a reason that is not a
// mill failure (an unverified dependency, conflicting dependencies); the run loop blocks it.
type blockedError struct{ reason string }

func (e *blockedError) Error() string { return e.reason }

func (p *Pipeline) logf(format string, a ...any) {
	if p.Out != nil {
		line := fmt.Sprintf(format+"\n", a...)
		p.outMu.Lock()
		defer p.outMu.Unlock()
		io.WriteString(p.Out, line)
	}
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Run executes the task until it is verified, blocked or failed. The caller decides whether
// the task may run (status pending, or a retry). A returned error means mill itself failed;
// the task is then marked failed when possible.
func (p *Pipeline) Run(ctx context.Context, taskID string) (Outcome, error) {
	t, err := p.Store.GetTask(ctx, taskID)
	if err != nil {
		return Outcome{}, err
	}
	out, err := p.run(ctx, t)
	if err != nil {
		reason := "mill error: " + err.Error()
		// Record the failure even if ctx is done.
		_ = p.Store.SetTaskStatus(context.WithoutCancel(ctx), t.ID, store.TaskFailed, "", reason)
		out.Status, out.Reason = store.TaskFailed, reason
	}
	out.Usage, _ = p.Store.TaskUsage(context.WithoutCancel(ctx), t.ID)
	switch out.Status {
	case store.TaskVerified:
		p.logf("%s: verified at %s (%d attempt(s) this run; task total %s)", t.ID, short(out.HeadSHA), out.Attempts, fmtUsage(out.Usage))
	default:
		p.logf("%s: %s: %s (task total %s)", t.ID, out.Status, out.Reason, fmtUsage(out.Usage))
	}
	return out, err
}

func (p *Pipeline) run(ctx context.Context, t store.Task) (Outcome, error) {
	if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhasePrepare, ""); err != nil {
		return Outcome{}, err
	}
	br, err := p.prepare(ctx, t)
	var be *blockedError
	if errors.As(err, &be) {
		return p.block(ctx, t.ID, store.PhasePrepare, be.reason, Outcome{}, nil)
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("prepare worktree: %w", err)
	}
	startSHA := br.StartSHA
	if startSHA == "" {
		startSHA = br.BaseSHA
	}
	p.logf("%s: worktree %s on %s (base %s @%s)", t.ID, br.Worktree, br.Name, br.Base, short(br.BaseSHA))

	prev, err := p.Store.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return Outcome{}, err
	}
	findings, err := p.lastFindings(ctx, t.ID)
	if err != nil {
		return Outcome{}, err
	}
	gateCmds := append([]string{p.Cfg.CheckCmd}, t.Checks...)
	limits := p.Cfg.AttemptBudget(t.Size)
	out := Outcome{}

	for n := prev + 1; n <= prev+p.Cfg.MaxAttempts; n++ {
		out.Attempts++
		logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n))

		// implement
		if blocked, reason, err := p.checkBudgets(ctx, t, limits); err != nil || blocked {
			return p.block(ctx, t.ID, store.PhaseImplement, reason, out, err)
		}
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseImplement, ""); err != nil {
			return out, err
		}
		implPrompt := prompt.Implementer(prompt.ImplementerInput{
			Task: t.Spec, GateCmds: nonEmpty(gateCmds), Attempt: n - prev, MaxAttempts: p.Cfg.MaxAttempts, Findings: findings,
		})
		ar, err := p.agent(ctx, t, n, runner.RoleImplementer, implPrompt, prompt.ImplementerSchema, ImplementerTools, limits, br.Worktree, logDir)
		if err != nil {
			return out, err
		}
		if ar.stop != "" {
			out.RateLimited, out.ResetsAt = ar.rateLimited, ar.res.ResetsAt
			return p.block(ctx, t.ID, store.PhaseImplement, ar.stop, out, nil)
		}

		var summary string
		findings = nil
		if ar.res.Status != runner.StatusOK {
			findings = append(findings, prompt.Finding{Source: "mill", Issue: "the implementer run failed: " + ar.res.Error})
		} else if impl, perr := prompt.ParseImplementer(ar.res.Output); perr != nil {
			findings = append(findings, prompt.Finding{Source: "mill", Issue: perr.Error()})
		} else {
			summary = impl.Summary
		}

		// commit whatever changed, even after a failed run, so the next attempt builds on it
		changed, err := gitx.HasChanges(ctx, br.Worktree)
		if err != nil {
			return out, err
		}
		if changed {
			msg := fmt.Sprintf("mill: %s attempt %d\n\n%s\n\nMill-Task: %s\nMill-Attempt: %d\n", t.ID, n, strings.TrimSpace(summary), t.ID, n)
			head, err := gitx.CommitAll(ctx, br.Worktree, msg)
			if err != nil {
				return out, fmt.Errorf("commit attempt %d: %w", n, err)
			}
			br.HeadSHA = head
			if err := p.Store.SaveBranch(ctx, br); err != nil {
				return out, err
			}
		}
		if br.HeadSHA == "" || br.HeadSHA == startSHA {
			findings = append(findings, prompt.Finding{Source: "mill", Issue: "no changes were made: the worktree is identical to its starting point"})
		}
		if len(findings) > 0 {
			if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
				return out, err
			}
			continue
		}

		// owns
		var outside []string
		if p.Cfg.OwnsPolicy != "off" {
			files, err := gitx.ChangedFiles(ctx, br.Worktree, startSHA)
			if err != nil {
				return out, fmt.Errorf("list changed files for attempt %d: %w", n, err)
			}
			outside = plan.OutsideOwns(t.Owns, files)
		}
		if len(outside) > 0 {
			if _, err := p.Store.AppendEvent(ctx, "owns-outside", "", t.ID, map[string]any{"attempt": n, "files": outside}); err != nil {
				return out, err
			}
			if p.Cfg.OwnsPolicy == "enforce" {
				for _, f := range outside {
					findings = append(findings, prompt.Finding{Source: "mill", File: f, Issue: fmt.Sprintf("changed outside the task's owns; revert it with `git restore --source %s -- %s` (delete it instead if the task created it), or edit the task to widen its owns", short(startSHA), f)})
				}
				p.logf("%s a%d owns      %d file(s) outside owns", t.ID, n, len(outside))
				if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
					return out, err
				}
				continue
			}
		}

		// gate
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseGate, ""); err != nil {
			return out, err
		}
		started := p.now()
		gopts := gate.Options{
			Host: p.Host, Dir: br.Worktree, Cmds: gateCmds, LogDir: logDir,
			Env: p.env(nil), Timeout: p.Cfg.GateTimeout.Duration,
		}
		g := gate.Run(ctx, gopts)
		if _, err := p.Store.AppendEvent(ctx, "task-gate", "", t.ID, map[string]any{"attempt": n, "head": br.HeadSHA, "result": g}); err != nil {
			return out, err
		}
		if ctx.Err() != nil {
			return p.block(ctx, t.ID, store.PhaseGate, "interrupted", out, nil)
		}
		var flaky []string
		if !g.Passed && len(g.Commands) == 0 {
			// No checks configured: nothing to triage.
			p.logf("%s a%d gate      FAIL %s (%s)", t.ID, n, g.Reason, p.since(started))
			findings = gateFindings(g)
			if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
				return out, err
			}
			continue
		}
		if !g.Passed {
			tr, err := p.triage(ctx, t, n, startSHA, gopts, g)
			if err != nil {
				return out, err
			}
			if ctx.Err() != nil {
				return p.block(ctx, t.ID, store.PhaseGate, "interrupted", out, nil)
			}
			if tr.blocked != "" {
				// Not a failed attempt: no retry, but the findings explain the block.
				if err := p.recordFindings(ctx, t.ID, n, tr.findings); err != nil {
					return out, err
				}
				p.logf("%s a%d gate      FAIL on the starting point too (%s)", t.ID, n, p.since(started))
				return p.block(ctx, t.ID, store.PhaseGate, tr.blocked, out, nil)
			}
			if !tr.gate.Passed {
				p.logf("%s a%d gate      FAIL %s (%s)", t.ID, n, tr.gate.Reason, p.since(started))
				findings = tr.findings
				if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
					return out, err
				}
				continue
			}
			g, flaky = tr.gate, tr.flaky
		}
		p.logf("%s a%d gate      ok (%d command(s), %s)", t.ID, n, len(g.Commands), p.since(started))
		br.GateSHA = br.HeadSHA
		if err := p.Store.SaveBranch(ctx, br); err != nil {
			return out, fmt.Errorf("record the gate result for attempt %d: %w", n, err)
		}

		// verify
		if blocked, reason, err := p.checkBudgets(ctx, t, limits); err != nil || blocked {
			return p.block(ctx, t.ID, store.PhaseVerify, reason, out, err)
		}
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseVerify, ""); err != nil {
			return out, err
		}
		diff, err := gitx.Diff(ctx, br.Worktree, startSHA)
		if err != nil {
			return out, err
		}
		stat, err := gitx.DiffStat(ctx, br.Worktree, startSHA)
		if err != nil {
			return out, err
		}
		if err := writeFile(filepath.Join(logDir, "diff.patch"), diff); err != nil {
			return out, err
		}
		verPrompt := prompt.Verifier(prompt.VerifierInput{Task: t.Spec, BaseSHA: startSHA, Diff: diff, DiffStat: stat, Gate: g, OutsideOwns: outside, Flaky: flaky})
		vr, err := p.agent(ctx, t, n, runner.RoleVerifier, verPrompt, prompt.VerifierSchema, VerifierTools, limits, br.Worktree, logDir)
		if err != nil {
			return out, err
		}
		if vr.stop != "" {
			out.RateLimited, out.ResetsAt = vr.rateLimited, vr.res.ResetsAt
			return p.block(ctx, t.ID, store.PhaseVerify, vr.stop, out, nil)
		}
		if vr.res.Status != runner.StatusOK {
			findings = []prompt.Finding{{Source: "mill", Issue: "the verifier could not complete its review: " + vr.res.Error}}
			if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
				return out, err
			}
			continue
		}
		verdict, perr := prompt.ParseVerifier(vr.res.Output)
		if perr != nil {
			findings = []prompt.Finding{{Source: "mill", Issue: "the verifier's answer was unusable: " + perr.Error()}}
			if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
				return out, err
			}
			continue
		}
		if _, err := p.Store.AppendEvent(ctx, "task-verdict", "", t.ID, map[string]any{"attempt": n, "head": br.HeadSHA, "verdict": verdict}); err != nil {
			return out, err
		}
		if verdict.Verdict == "pass" {
			p.logf("%s a%d verdict   pass: %s", t.ID, n, oneLine(verdict.Summary))
			if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskVerified, "", ""); err != nil {
				return out, err
			}
			out.Status, out.HeadSHA = store.TaskVerified, br.HeadSHA
			return out, nil
		}
		p.logf("%s a%d verdict   fail: %d finding(s)", t.ID, n, len(verdict.Findings))
		findings = verdict.Findings
		if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
			return out, err
		}
	}
	return p.block(ctx, t.ID, "", fmt.Sprintf("not verified after %d attempt(s); see `mill task show %s` for the last findings", p.Cfg.MaxAttempts, t.ID), out, nil)
}

// prepare creates (or reattaches) the task's branch and worktree (TP1).
func (p *Pipeline) prepare(ctx context.Context, t store.Task) (store.Branch, error) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	name := BranchName(t.ID)
	wt := filepath.Join(p.Cfg.Worktrees(p.Paths.Root, p.Home), t.ID)

	br, err := p.Store.GetBranch(ctx, t.ID)
	switch {
	case err == nil:
		// A retry: keep the original base so diffs stay comparable (GT3).
		if br.Worktree != "" {
			wt = br.Worktree
		}
		if !gitx.IsWorktreeOn(ctx, wt, name) {
			if !gitx.BranchExists(ctx, p.Paths.Root, name) {
				return br, fmt.Errorf("branch %s is recorded but missing from git", name)
			}
			if err := gitx.PruneWorktrees(ctx, p.Paths.Root); err != nil {
				return br, err
			}
			if _, statErr := os.Stat(wt); statErr == nil {
				return br, fmt.Errorf("%s exists but is not a worktree on %s; move it away and retry", wt, name)
			}
			if err := gitx.AddWorktreeForBranch(ctx, p.Paths.Root, wt, name); err != nil {
				return br, err
			}
		}
		br.Worktree = wt
		// A killed run may have left uncommitted work; keep it so the next attempt builds on it.
		changed, err := gitx.HasChanges(ctx, wt)
		if err != nil {
			return br, err
		}
		if changed {
			msg := fmt.Sprintf("mill: %s interrupted attempt\n\nMill-Task: %s\n", t.ID, t.ID)
			if _, err := gitx.CommitAll(ctx, wt, msg); err != nil {
				return br, fmt.Errorf("commit interrupted work in %s: %w", wt, err)
			}
			p.logf("%s: committed uncommitted work from an interrupted attempt", t.ID)
		}
		if head, err := gitx.RevParse(ctx, wt, "HEAD"); err == nil {
			br.HeadSHA = head
		}
	case errors.Is(err, store.ErrNotFound):
		if gitx.BranchExists(ctx, p.Paths.Root, name) {
			return br, fmt.Errorf("branch %s already exists in git but mill has no record of it; delete or rename it", name)
		}
		if _, statErr := os.Stat(wt); statErr == nil {
			return br, fmt.Errorf("worktree path %s already exists; move it away and retry", wt)
		}
		base, baseSHA, merges, err := p.baseFor(ctx, t)
		if err != nil {
			return br, err
		}
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			return br, err
		}
		if err := gitx.AddWorktree(ctx, p.Paths.Root, wt, name, baseSHA); err != nil {
			return br, err
		}
		for _, m := range merges {
			err := gitx.Merge(ctx, wt, m.rev, m.message)
			if err == nil {
				continue
			}
			var ce *gitx.ConflictError
			if !errors.As(err, &ce) {
				return br, fmt.Errorf("merge dependency %s: %w", m.id, err)
			}
			// Leave nothing behind so a retry starts clean.
			if rmErr := gitx.RemoveWorktree(ctx, p.Paths.Root, wt); rmErr != nil {
				return br, fmt.Errorf("dependency %s conflicts in %s; removing the worktree %s failed: %w", m.id, strings.Join(ce.Files, ", "), wt, rmErr)
			}
			if delErr := gitx.DeleteBranch(ctx, p.Paths.Root, name); delErr != nil {
				return br, fmt.Errorf("dependency %s conflicts in %s; deleting branch %s failed: %w", m.id, strings.Join(ce.Files, ", "), name, delErr)
			}
			return br, &blockedError{fmt.Sprintf("dependency %s (branch %s) conflicts with the other dependencies in %s; resolve the conflict in the plan or by hand",
				m.id, m.branch, strings.Join(ce.Files, ", "))}
		}
		startSHA, err := gitx.RevParse(ctx, wt, "HEAD")
		if err != nil {
			return br, err
		}
		br = store.Branch{TaskID: t.ID, Name: name, Base: base, BaseSHA: baseSHA, StartSHA: startSHA, HeadSHA: startSHA, Worktree: wt}
	default:
		return br, err
	}
	return br, p.Store.SaveBranch(ctx, br)
}

type mergeDep struct{ id, branch, rev, message string }

// baseFor decides what a new task branch starts from (see the goal base rules, worked out by
// fanin.Start) and what must be merged into it afterwards. A landed dependency's code is in
// the base branch, so its own branch is never used.
func (p *Pipeline) baseFor(ctx context.Context, t store.Task) (base, baseSHA string, merges []mergeDep, err error) {
	tg, err := fanin.Start(ctx, p.Store, p.Cfg, p.Paths.Root, t)
	if err != nil {
		return "", "", nil, err
	}
	if tg.Wait != "" {
		return "", "", nil, &blockedError{tg.Wait}
	}
	if tg.Warning != "" {
		p.logf("%s: warning: %s", t.ID, tg.Warning)
	}
	name := BranchName(t.ID)
	for _, m := range tg.Merges() {
		msg := fmt.Sprintf("mill: merge %s (dependency %s) into %s", m.Branch, m.ID, name)
		if tg.Upstream != nil && m.Branch == tg.Upstream.Branch {
			msg = fmt.Sprintf("mill: merge %s (landed dependencies) into %s", m.Branch, name)
		}
		merges = append(merges, mergeDep{id: m.ID, branch: m.Branch, rev: m.Head, message: msg})
	}
	return tg.Base, tg.BaseSHA, merges, nil
}

// agentRun is the outcome of one agent run.
type agentRun struct {
	res  runner.Result
	stop string // non-empty: the task must stop (budget exceeded, rate limited, interrupted, timed out)
	// rateLimited is true when the run hit the account's usage limit.
	rateLimited bool
}

// agent runs one agent with budget enforcement (BG1) and records the attempt.
func (p *Pipeline) agent(ctx context.Context, t store.Task, n int, role runner.Role, promptText string,
	schema json.RawMessage, tools []string, limits budget.Limits, dir, logDir string) (agentRun, error) {
	id, err := p.Store.StartAttempt(ctx, t.ID, n, string(role))
	if err != nil {
		return agentRun{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, p.Cfg.Runner.AttemptTimeout.Duration)
	defer cancel()

	var mu sync.Mutex
	var spent budget.Usage
	var over string
	var startErr error
	started := p.now()
	live := p.startLiveUsage(ctx, id, t.ID, p.Cfg.Model(t.Size))
	defer live.stop() // also on a runner panic; the normal path stops it explicitly below
	res, runErr := p.Runner.Run(rctx, runner.Request{
		Role: role, TaskID: t.ID, Prompt: promptText, Dir: dir, Schema: schema, Tools: tools,
		Model: p.Cfg.Model(t.Size), Budget: limits, Count: p.Cfg.Budget.Count,
		Name:   fmt.Sprintf("%s a%d %s", t.ID, n, role),
		LogDir: logDir, ProtectedPaths: []string{p.Paths.MillDir}, Env: p.env(p.AgentEnvAllow),
		OnStart: func(st runner.Started) {
			if err := p.Store.SetAttemptProcess(context.WithoutCancel(ctx), id, st.PID, st.Command, st.RawPath); err != nil {
				mu.Lock()
				startErr = err
				mu.Unlock()
			}
		},
		OnUsage: func(d budget.Usage) {
			mu.Lock()
			defer mu.Unlock()
			spent = spent.Add(d)
			live.offer(spent)
			if exceeded, why := budget.Exceeded(spent, limits, p.Cfg.Budget.Count); exceeded && over == "" {
				over = why
				cancel()
			}
		},
	})
	live.stop()
	mu.Lock()
	overReason := over
	recErr := startErr
	mu.Unlock()

	status := string(res.Status)
	if overReason != "" && res.Status == runner.StatusOK {
		// The run finished before mill could stop it; keep the work, flag the overrun.
		res.Warnings = append(res.Warnings, "limit crossed as the run finished: "+overReason)
	}
	ar := agentRun{res: res}
	switch {
	case runErr != nil:
		status = "error"
	case res.Status == runner.StatusBudgetExceeded:
		ar.stop = "budget exceeded in attempt " + fmt.Sprint(n) + " (" + string(role) + "): " + res.Error
	case overReason != "" && res.Status != runner.StatusOK:
		status = string(runner.StatusBudgetExceeded)
		ar.stop = fmt.Sprintf("budget exceeded in attempt %d (%s): %s", n, role, overReason)
	case res.Status == runner.StatusRateLimited:
		ar.rateLimited = true
		ar.stop = RateLimitReason(res.LimitType, res.ResetsAt, p.now())
	case res.Status == runner.StatusCancelled && ctx.Err() != nil:
		ar.stop = "interrupted"
	case res.Status == runner.StatusCancelled && errors.Is(rctx.Err(), context.DeadlineExceeded):
		status = "timeout"
		ar.stop = fmt.Sprintf("%s timed out after %s in attempt %d", role, p.Cfg.Runner.AttemptTimeout.Duration, n)
	}

	outPath := ""
	if len(res.Output) > 0 {
		outPath = filepath.Join(logDir, string(role)+".output.json")
		if err := writeFile(outPath, string(res.Output)); err != nil {
			return ar, err
		}
	}
	// Record the attempt even when ctx is done, so spend is never lost.
	if err := p.Store.FinishAttempt(context.WithoutCancel(ctx), id, status, res.Usage, res.CostEstimated, outPath, res.RawPath); err != nil {
		return ar, err
	}
	detail := ""
	if res.Error != "" && res.Status != runner.StatusOK {
		detail = ": " + oneLine(res.Error)
	}
	if len(res.Warnings) > 0 {
		detail += " [" + strings.Join(res.Warnings, "; ") + "]"
	}
	p.logf("%s a%d %-9s %s %s (%s)%s", t.ID, n, role, status, fmtUsageEst(res.Usage, res.CostEstimated), p.since(started), detail)
	if runErr != nil {
		return ar, fmt.Errorf("%s run: %w", role, runErr)
	}
	if recErr != nil {
		return ar, fmt.Errorf("%s run: %w", role, recErr)
	}
	return ar, nil
}

// RateLimitReason is the block reason for a run that hit the account's usage limit, e.g.
// "rate limited: the five_hour usage limit was reached; resets at 12:50; run again after that".
func RateLimitReason(limitType string, resetsAt, now time.Time) string {
	what := "a usage limit was reached"
	if limitType != "" {
		what = "the " + limitType + " usage limit was reached"
	}
	if resetsAt.IsZero() {
		return "rate limited: " + what + "; the reset time is unknown; run again later"
	}
	return "rate limited: " + what + "; resets at " + FormatResetTime(resetsAt, now) + "; run again after that"
}

// FormatResetTime formats a reset time in local time: "15:04" when it is on the same day as
// now, "Jan 2 15:04" otherwise.
func FormatResetTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// liveUsageInterval is the minimum time between live usage writes for one attempt.
var liveUsageInterval = 2 * time.Second

// liveUsage writes a running attempt's usage total to the store, at most once per interval,
// from its own goroutine so the agent's usage callback never waits on the database.
type liveUsage struct {
	latest chan budget.Usage // capacity 1: holds the newest unwritten total
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (p *Pipeline) startLiveUsage(ctx context.Context, id int64, taskID, model string) *liveUsage {
	l := &liveUsage{latest: make(chan budget.Usage, 1), quit: make(chan struct{}), done: make(chan struct{})}
	wctx := context.WithoutCancel(ctx)
	go func() {
		defer close(l.done)
		warned := false
		for {
			select {
			case <-l.quit:
				return
			case u := <-l.latest:
				est := false
				if usd, ok := price.Estimate(model, u, p.Cfg.Prices); ok {
					u.CostUSD, est = usd, true
				} else {
					u.CostUSD = 0
				}
				if err := p.Store.UpdateAttemptUsage(wctx, id, u, est); err != nil && !warned {
					warned = true
					p.logf("%s: live usage not recorded: %v", taskID, err)
				}
				select {
				case <-l.quit:
					return
				case <-time.After(liveUsageInterval):
				}
			}
		}
	}()
	return l
}

// offer hands the writer the latest running total without blocking.
func (l *liveUsage) offer(u budget.Usage) {
	select {
	case <-l.latest: // drop the stale total
	default:
	}
	select {
	case l.latest <- u:
	default:
	}
}

// stop ends the writer and waits for any write in flight, so it cannot land after FinishAttempt.
func (l *liveUsage) stop() {
	l.once.Do(func() { close(l.quit) })
	<-l.done
}

// checkBudgets blocks when the task budget, or for a goal task the goal budget, cannot cover
// another attempt (BG2).
func (p *Pipeline) checkBudgets(ctx context.Context, t store.Task, need budget.Limits) (bool, string, error) {
	if blocked, reason, err := p.checkTaskBudget(ctx, t.ID, need); err != nil || blocked {
		return blocked, reason, err
	}
	goal, _ := store.SplitTaskID(t.ID)
	if goal == "" {
		return false, "", nil
	}
	spent, err := p.Store.GoalUsage(ctx, goal)
	if err != nil {
		return false, "", err
	}
	if !budget.Covers(p.Cfg.Budget.Goal, spent, need, p.Cfg.Budget.Count) {
		return true, fmt.Sprintf("goal budget exhausted for %s: spent %s of %s, an attempt may use %s",
			goal, fmtUsage(spent), fmtLimits(p.Cfg.Budget.Goal), fmtLimits(need)), nil
	}
	return false, "", nil
}

func (p *Pipeline) checkTaskBudget(ctx context.Context, taskID string, need budget.Limits) (bool, string, error) {
	spent, err := p.Store.TaskUsage(ctx, taskID)
	if err != nil {
		return false, "", err
	}
	if !budget.Covers(p.Cfg.Budget.Task, spent, need, p.Cfg.Budget.Count) {
		return true, fmt.Sprintf("task budget cannot cover another attempt: spent %s of %s, an attempt may use %s",
			fmtUsage(spent), fmtLimits(p.Cfg.Budget.Task), fmtLimits(need)), nil
	}
	return false, "", nil
}

func (p *Pipeline) block(ctx context.Context, taskID, phase, reason string, out Outcome, err error) (Outcome, error) {
	if err != nil {
		return out, err
	}
	if e := p.Store.SetTaskStatus(context.WithoutCancel(ctx), taskID, store.TaskBlocked, phase, reason); e != nil {
		return out, e
	}
	out.Status, out.Reason = store.TaskBlocked, reason
	return out, nil
}

func (p *Pipeline) recordFindings(ctx context.Context, taskID string, n int, f []prompt.Finding) error {
	_, err := p.Store.AppendEvent(ctx, "attempt-findings", "", taskID, map[string]any{"attempt": n, "findings": f})
	return err
}

// lastFindings returns the findings of the most recent failed attempt (for a retry).
func (p *Pipeline) lastFindings(ctx context.Context, taskID string) ([]prompt.Finding, error) {
	return LastFindings(ctx, p.Store, taskID)
}

// LastFindings returns the findings recorded for a task's most recent failed attempt.
func LastFindings(ctx context.Context, s *store.Store, taskID string) ([]prompt.Finding, error) {
	evs, err := s.TaskEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "attempt-findings" {
			continue
		}
		var d struct {
			Findings []prompt.Finding `json:"findings"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil {
			return nil, err
		}
		return d.Findings, nil
	}
	return nil, nil
}

func (p *Pipeline) env(extra []string) []string {
	allow := append(append(append([]string{}, host.DefaultEnvAllow...), p.Cfg.EnvAllow...), extra...)
	return host.AllowEnv(os.Environ(), allow)
}

func (p *Pipeline) since(t time.Time) string {
	return p.now().Sub(t).Round(100 * time.Millisecond).String()
}

func gateFindings(g gate.Result) []prompt.Finding {
	if len(g.Commands) == 0 {
		return []prompt.Finding{{Source: "gate", Issue: g.Reason}}
	}
	var f []prompt.Finding
	for _, c := range g.Commands {
		if !c.Passed() {
			f = append(f, cmdFinding(c))
		}
	}
	return f
}

// cmdFinding describes one failed gate command.
func cmdFinding(c gate.CmdResult) prompt.Finding {
	what := fmt.Sprintf("exit %d", c.Exit)
	if c.Error != "" {
		what = c.Error
	}
	issue := fmt.Sprintf("`%s` failed (%s)", c.Cmd, what)
	if c.Tail != "" {
		issue += ". Output (last lines):\n" + indent(c.Tail)
	}
	return prompt.Finding{Source: "gate", Issue: issue}
}

func nonEmpty(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func fmtUsage(u budget.Usage) string { return fmtUsageEst(u, false) }

// fmtUsageEst formats usage, marking an estimated cost with "~".
func fmtUsageEst(u budget.Usage, estimated bool) string {
	mark := ""
	if estimated {
		mark = "~"
	}
	return fmt.Sprintf("%s tok, %s$%.2f", fmtTokens(u.Tokens(budget.CountBillable)), mark, u.CostUSD)
}

func fmtLimits(l budget.Limits) string {
	var parts []string
	if l.Tokens > 0 {
		parts = append(parts, fmtTokens(l.Tokens)+" tok")
	}
	if l.USD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", l.USD))
	}
	if len(parts) == 0 {
		return "unlimited"
	}
	return strings.Join(parts, " / ")
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
