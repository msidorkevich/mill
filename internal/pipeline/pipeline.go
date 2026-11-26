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
	"github.com/msidorkevich/mill/internal/skiptest"
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
	// LiveUsageInterval is the minimum time between live usage writes for one attempt;
	// zero means DefaultLiveUsageInterval.
	LiveUsageInterval time.Duration
	// MaxParallelGates bounds the gates running at the same time across every mill process
	// on this database; zero means Cfg.MaxParallelGates (and 1 when that is unset too).
	// Agents are not limited by it.
	MaxParallelGates int
	// GateSlotPoll is how often a gate waiting for a slot checks again; zero means
	// DefaultGateSlotPoll.
	GateSlotPoll time.Duration

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

// AgentSlot is a task's hold on one of the scheduler's agent slots (max_parallel). A task
// starts holding it; the pipeline releases it while the task waits for and runs its gate and
// acquires it again before the next agent phase. Acquire waits for a free slot or for ctx to
// end; both calls do nothing when the slot is already in that state.
type AgentSlot interface {
	Acquire(ctx context.Context) error
	Release()
}

type agentSlotKey struct{}

// WithAgentSlot returns ctx carrying the task's agent slot for Run.
func WithAgentSlot(ctx context.Context, slot AgentSlot) context.Context {
	return context.WithValue(ctx, agentSlotKey{}, slot)
}

// noSlot is the agent slot of a run without a scheduler: agents are not limited.
type noSlot struct{}

func (noSlot) Acquire(context.Context) error { return nil }
func (noSlot) Release()                      {}

// AgentSlotFrom returns the agent slot ctx carries; without one, a slot that never waits.
func AgentSlotFrom(ctx context.Context) AgentSlot {
	if s, ok := ctx.Value(agentSlotKey{}).(AgentSlot); ok {
		return s
	}
	return noSlot{}
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
// mill failure (an unverified dependency, a dependency conflict the resolver could not
// resolve); the run loop blocks it.
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
	var se *stoppedError
	if errors.As(err, &se) {
		return p.block(ctx, t.ID, store.PhasePrepare, se.stop, Outcome{RateLimited: se.rateLimited, ResetsAt: se.resetsAt}, nil)
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

	// A task blocked after its gate passed, with nothing changed since, resumes at the verifier.
	out, verFindings, resumed, done, err := p.resume(ctx, t, br, startSHA, prev, limits, out)
	if err != nil || done {
		return out, err
	}
	if resumed {
		findings = verFindings
	}

	slot := AgentSlotFrom(ctx)
	for n := prev + 1; n <= prev+p.Cfg.MaxAttempts; n++ {
		out.Attempts++
		logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n))

		// An attempt after a failed gate waits for an agent slot.
		if err := slot.Acquire(ctx); err != nil {
			if ctx.Err() != nil {
				return p.block(ctx, t.ID, store.PhaseImplement, "interrupted", out, nil)
			}
			return out, fmt.Errorf("wait for an agent slot for attempt %d: %w", n, err)
		}

		// implement
		if blocked, reason, err := p.checkBudgets(ctx, t, stepImplement); err != nil || blocked {
			return p.block(ctx, t.ID, store.PhaseImplement, reason, out, err)
		}
		implModel, implWhy, err := p.implementerModel(ctx, t, n)
		if err != nil {
			return out, err
		}
		if implModel == "" {
			return p.block(ctx, t.ID, store.PhaseImplement, implWhy, out, nil)
		}
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseImplement, ""); err != nil {
			return out, err
		}
		implPrompt := prompt.Implementer(prompt.ImplementerInput{
			Task: t.Spec, CheckCmd: p.Cfg.CheckCmd, RunsCheck: p.Cfg.ImplementerRunsCheck, Attempt: n - prev, MaxAttempts: p.Cfg.MaxAttempts, Findings: findings,
		})
		ar, err := p.agent(ctx, t, n, runner.RoleImplementer, implModel, implWhy, implPrompt, prompt.ImplementerSchema, ImplementerTools, limits, br.Worktree, logDir)
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
		outside, err := p.outsideOwns(ctx, t, br.Worktree, startSHA, n)
		if err != nil {
			return out, err
		}
		if len(outside) > 0 {
			owners, err := p.siblingOwners(ctx, t, outside)
			if err != nil {
				return out, err
			}
			if _, err := p.Store.AppendEvent(ctx, "owns-outside", "", t.ID, map[string]any{"attempt": n, "files": outside, "owners": owners}); err != nil {
				return out, err
			}
			if len(owners) > 0 || p.Cfg.OwnsPolicy == "enforce" {
				var ownerIDs []string
				seen := map[string]bool{}
				for _, f := range outside {
					ids, owned := owners[f]
					if owned {
						for _, id := range ids {
							if !seen[id] {
								seen[id] = true
								ownerIDs = append(ownerIDs, id)
							}
						}
						findings = append(findings, prompt.Finding{Source: "mill", File: f, Issue: fmt.Sprintf("changed a file owned by task %s; revert it with `git restore --source %s -- %s` (delete it instead if the task created it), or edit the task to widen its owns", strings.Join(ids, ", "), short(startSHA), f)})
					} else if p.Cfg.OwnsPolicy == "enforce" {
						findings = append(findings, prompt.Finding{Source: "mill", File: f, Issue: fmt.Sprintf("changed outside the task's owns; revert it with `git restore --source %s -- %s` (delete it instead if the task created it), or edit the task to widen its owns", short(startSHA), f)})
					}
				}
				if len(owners) > 0 {
					p.logf("%s a%d owns      %d file(s) owned by %s", t.ID, n, len(owners), strings.Join(ownerIDs, ", "))
				} else {
					p.logf("%s a%d owns      %d file(s) outside owns", t.ID, n, len(outside))
				}
				if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
					return out, err
				}
				continue
			}
		}

		// gate
		gopts := gate.Options{
			Host: p.Host, Fallback: host.NewExec(), Dir: br.Worktree, Cmds: gateCmds, LogDir: logDir,
			Env: p.env(nil), Timeout: p.Cfg.GateTimeout.Duration, Nice: p.Cfg.GateNice,
			Label: fmt.Sprintf("%s a%d gate", t.ID, n),
		}
		// The gate runs no agent: its agent slot is free for another task meanwhile.
		slot.Release()
		gr, err := p.gateInSlot(ctx, t, n, startSHA, br.HeadSHA, gopts)
		if err != nil {
			return out, err
		}
		if ctx.Err() != nil {
			return p.block(ctx, t.ID, store.PhaseGate, "interrupted", out, nil)
		}
		g, started := gr.gate, gr.started
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
			tr := gr.triage
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
		if err := slot.Acquire(ctx); err != nil {
			if ctx.Err() != nil {
				return p.block(ctx, t.ID, store.PhaseVerify, "interrupted", out, nil)
			}
			return out, fmt.Errorf("wait for an agent slot for the verifier of attempt %d: %w", n, err)
		}
		var done bool
		out, findings, done, err = p.verify(ctx, t, verifyStep{
			n: n, br: br, startSHA: startSHA, logDir: logDir, gate: g, outside: outside, flaky: flaky,
			implModel: implModel, limits: limits,
		}, out)
		if err != nil || done {
			return out, err
		}
	}
	return p.block(ctx, t.ID, "", fmt.Sprintf("not verified after %d attempt(s); see `mill task show %s` for the last findings", p.Cfg.MaxAttempts, t.ID), out, nil)
}

// outsideOwns lists the files changed since startSHA in wt that are outside the task's owns;
// none when the owns policy is off.
func (p *Pipeline) outsideOwns(ctx context.Context, t store.Task, wt, startSHA string, n int) ([]string, error) {
	if p.Cfg.OwnsPolicy == "off" {
		return nil, nil
	}
	files, err := gitx.ChangedFiles(ctx, wt, startSHA)
	if err != nil {
		return nil, fmt.Errorf("list changed files for attempt %d: %w", n, err)
	}
	return plan.OutsideOwns(t.Owns, files), nil
}

// verifyStep is what the verifier of attempt n reviews: the branch at br.HeadSHA against
// startSHA, with the gate result that passed on it.
type verifyStep struct {
	n         int
	br        store.Branch
	startSHA  string
	logDir    string
	gate      gate.Result
	outside   []string // changed files outside the task's owns
	flaky     []string // gate commands that passed only on a re-run
	implModel string   // the implementer's model, which picks the verifier's
	limits    budget.Limits
}

// verify runs the verify step of an attempt: the verifier budget check, the verifier and its
// verdict. done means the run ends with out (verified or blocked) or err. Otherwise the
// verifier did not pass the change: findings are recorded and the next attempt starts with them.
func (p *Pipeline) verify(ctx context.Context, t store.Task, v verifyStep, out Outcome) (_ Outcome, findings []prompt.Finding, done bool, err error) {
	n := v.n
	if blocked, reason, err := p.checkBudgets(ctx, t, stepVerify); err != nil || blocked {
		out, err = p.block(ctx, t.ID, store.PhaseVerify, reason, out, err)
		return out, nil, true, err
	}
	if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseVerify, ""); err != nil {
		return out, nil, true, err
	}
	diff, err := gitx.Diff(ctx, v.br.Worktree, v.startSHA)
	if err != nil {
		return out, nil, true, err
	}
	stat, err := gitx.DiffStat(ctx, v.br.Worktree, v.startSHA)
	if err != nil {
		return out, nil, true, err
	}
	if err := writeFile(filepath.Join(v.logDir, "diff.patch"), diff); err != nil {
		return out, nil, true, err
	}
	sibs, err := p.siblings(ctx, t)
	if err != nil {
		return out, nil, true, err
	}
	bc, err := p.baseChanges(ctx, t, v.br, n)
	if err != nil {
		return out, nil, true, err
	}
	verPrompt := prompt.Verifier(prompt.VerifierInput{Task: t.Spec, BaseSHA: v.startSHA, Diff: diff, DiffStat: stat, Gate: v.gate, OutsideOwns: v.outside, Flaky: v.flaky, Siblings: sibs, BaseChanged: bc.Files, BaseChangedStat: bc.Stat})
	verModel, verWhy := p.verifierModel(v.implModel)
	vr, err := p.agent(ctx, t, n, runner.RoleVerifier, verModel, verWhy, verPrompt, prompt.VerifierSchema, VerifierTools, v.limits, v.br.Worktree, v.logDir)
	if err != nil {
		return out, nil, true, err
	}
	if vr.stop != "" {
		out.RateLimited, out.ResetsAt = vr.rateLimited, vr.res.ResetsAt
		out, err = p.block(ctx, t.ID, store.PhaseVerify, vr.stop, out, nil)
		return out, nil, true, err
	}
	if vr.res.Status != runner.StatusOK {
		findings = []prompt.Finding{{Source: "mill", Issue: "the verifier could not complete its review: " + vr.res.Error}}
		if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
			return out, nil, true, err
		}
		return out, findings, false, nil
	}
	verdict, perr := prompt.ParseVerifier(vr.res.Output)
	if perr != nil {
		findings = []prompt.Finding{{Source: "mill", Issue: "the verifier's answer was unusable: " + perr.Error()}}
		if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
			return out, nil, true, err
		}
		return out, findings, false, nil
	}
	if _, err := p.Store.AppendEvent(ctx, "task-verdict", "", t.ID, map[string]any{"attempt": n, "head": v.br.HeadSHA, "verdict": verdict}); err != nil {
		return out, nil, true, err
	}
	if verdict.Verdict == "pass" {
		p.logf("%s a%d verdict   pass: %s", t.ID, n, oneLine(verdict.Summary))
		if err := p.recordSkippedTests(ctx, t.ID, n, v.br.Worktree, v.startSHA, v.br.HeadSHA); err != nil {
			return out, nil, true, err
		}
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskVerified, "", ""); err != nil {
			return out, nil, true, err
		}
		out.Status, out.HeadSHA = store.TaskVerified, v.br.HeadSHA
		return out, nil, true, nil
	}
	findings, outScope, owners, err := p.splitOutOfScope(ctx, t, verdict.Findings)
	if err != nil {
		return out, nil, true, err
	}
	if len(outScope) > 0 {
		if _, err := p.Store.AppendEvent(ctx, "verifier-out-of-scope", "", t.ID, map[string]any{"attempt": n, "findings": outScope, "owners": owners}); err != nil {
			return out, nil, true, err
		}
		p.logf("%s a%d verdict   %d finding(s) in files other tasks own, left to them", t.ID, n, len(outScope))
	}
	if len(outScope) > 0 && len(findings) == 0 {
		p.logf("%s a%d verdict   pass with notes: %d out-of-scope finding(s)", t.ID, n, len(outScope))
		if err := p.recordSkippedTests(ctx, t.ID, n, v.br.Worktree, v.startSHA, v.br.HeadSHA); err != nil {
			return out, nil, true, err
		}
		if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskVerified, "", ""); err != nil {
			return out, nil, true, err
		}
		out.Status, out.HeadSHA = store.TaskVerified, v.br.HeadSHA
		return out, nil, true, nil
	}
	p.logf("%s a%d verdict   fail: %d finding(s)", t.ID, n, len(findings))
	if err := p.recordFindings(ctx, t.ID, n, findings); err != nil {
		return out, nil, true, err
	}
	return out, findings, false, nil
}

// SkippedTestsEvent is the kind of the event listing the test skips a task added.
// OldSkippedTestsEvent is the kind earlier versions wrote; readers accept both.
const (
	SkippedTestsEvent    = "skipped-tests"
	OldSkippedTestsEvent = "skipped_tests"
)

// baseChanges finds what changed on t's base since it started that concerns it, for the
// verifier of attempt n, and records a base-changed event when there is anything.
func (p *Pipeline) baseChanges(ctx context.Context, t store.Task, br store.Branch, n int) (BaseChanges, error) {
	bc, err := FindBaseChanges(ctx, p.Paths.Root, p.Cfg, t, br)
	if err != nil || len(bc.Files) == 0 {
		return bc, err
	}
	if _, err := p.Store.AppendEvent(ctx, "base-changed", "", t.ID, map[string]any{"attempt": n, "base": bc.Base, "from": bc.From, "to": bc.To, "files": bc.Files}); err != nil {
		return BaseChanges{}, fmt.Errorf("record the base changes of task %s; check the mill database and run the verification again: %w", t.ID, err)
	}
	return bc, nil
}

// recordSkippedTests appends a skipped-tests event listing the test skips the task added
// between startSHA and head, when there are any. When there are none but an earlier attempt
// recorded skips, it appends an empty event so readers stop showing the stale ones. A
// detection failure is returned: the task is not verified without the check.
func (p *Pipeline) recordSkippedTests(ctx context.Context, taskID string, n int, dir, startSHA, head string) error {
	skips, err := skiptest.Added(ctx, dir, startSHA, head)
	if err != nil {
		return fmt.Errorf("%s: detect skipped tests: %w", taskID, err)
	}
	if len(skips) == 0 {
		stale, err := p.hasRecordedSkips(ctx, taskID)
		if err != nil || !stale {
			return err
		}
		p.logf("%s a%d added no test skips: clearing the skips an earlier attempt recorded", taskID, n)
		_, err = p.Store.AppendEvent(ctx, SkippedTestsEvent, "", taskID, map[string]any{"attempt": n, "head": head, "skips": []skiptest.Skip{}})
		return err
	}
	p.logf("%s a%d added %d test skip(s)", taskID, n, len(skips))
	_, err = p.Store.AppendEvent(ctx, SkippedTestsEvent, "", taskID, map[string]any{"attempt": n, "head": head, "skips": skips})
	return err
}

// hasRecordedSkips reports whether the task's latest skipped-tests event (either kind) lists
// any skips.
func (p *Pipeline) hasRecordedSkips(ctx context.Context, taskID string) (bool, error) {
	evs, err := p.Store.TaskEvents(ctx, taskID)
	if err != nil {
		return false, fmt.Errorf("%s: read skipped-tests events: %w", taskID, err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != SkippedTestsEvent && evs[i].Kind != OldSkippedTestsEvent {
			continue
		}
		var d struct {
			Skips []skiptest.Skip `json:"skips"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil {
			return false, fmt.Errorf("%s: corrupt skipped-tests event %d: %w", taskID, evs[i].Seq, err)
		}
		return len(d.Skips) > 0, nil
	}
	return false, nil
}

// DefaultGateSlotPoll is how often a gate waiting for a slot checks again when
// Pipeline.GateSlotPoll is zero.
const DefaultGateSlotPoll = 100 * time.Millisecond

func (p *Pipeline) maxParallelGates() int {
	switch {
	case p.MaxParallelGates > 0:
		return p.MaxParallelGates
	case p.Cfg.MaxParallelGates > 0:
		return p.Cfg.MaxParallelGates
	}
	return 1
}

func (p *Pipeline) gateSlotPoll() time.Duration {
	if p.GateSlotPoll > 0 {
		return p.GateSlotPoll
	}
	return DefaultGateSlotPoll
}

// AcquireGateSlot takes one of the max_parallel_gates gate slots for h, waiting while every
// slot is held; onWait (if not nil) is called once when it has to wait. Call the returned
// release once the gate is done, on every path.
func (p *Pipeline) AcquireGateSlot(ctx context.Context, h store.GateHolder, onWait func()) (release func() error, err error) {
	if h.PID == 0 {
		h.PID = os.Getpid()
	}
	return p.Store.AcquireGateSlot(ctx, h, p.maxParallelGates(), p.gateSlotPoll(), onWait)
}

// gateRun is the outcome of a task's gate, with its triage when it failed.
type gateRun struct {
	gate    gate.Result
	triage  triageResult // set when the gate failed with commands that ran
	started time.Time    // when the slot was taken and the gate began
}

// gateInSlot runs the task's gate, and the triage re-runs when it fails, inside one gate
// slot. While it waits for the slot the task is in phase PhaseGateWait, then PhaseGate. The
// slot is released on every path. The caller checks ctx afterwards: an interrupted gate (or
// wait for a slot) is meaningless.
func (p *Pipeline) gateInSlot(ctx context.Context, t store.Task, n int, startSHA, head string, gopts gate.Options) (gr gateRun, err error) {
	// The same tree gated with the same commands already passed: reuse it without a slot.
	tree, err := gitx.HeadTree(ctx, gopts.Dir)
	if err != nil {
		return gr, fmt.Errorf("gate of attempt %d: %w", n, err)
	}
	fp := store.GateFingerprint(p.Cfg.CheckCmd, t.Checks)
	reused, ok, err := p.reusedGate(ctx, t, n, head, tree, fp)
	if err != nil || ok {
		gr.gate, gr.started = reused, p.now()
		return gr, err
	}
	defer func() {
		// Only a gate that passed outright is recorded: a failure, or a pass only on a triage
		// re-run, is never reused.
		if err == nil && ctx.Err() == nil && gr.gate.Passed {
			err = p.recordGatePass(ctx, t, tree, fp, gr.gate)
		}
	}()
	var waitErr error
	release, err := p.AcquireGateSlot(ctx, store.GateHolder{TaskID: t.ID, Attempt: n}, func() {
		waitErr = p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseGateWait, "")
		p.logf("%s a%d gate      waiting for a free gate slot (max_parallel_gates = %d)", t.ID, n, p.maxParallelGates())
	})
	if err != nil {
		if ctx.Err() != nil && waitErr == nil {
			// Interrupted while waiting for a slot: no slot was taken.
			return gr, nil
		}
		return gr, errors.Join(err, waitErr)
	}
	defer func() {
		if rerr := release(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	if waitErr != nil {
		return gr, waitErr
	}
	if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseGate, ""); err != nil {
		return gr, err
	}
	gr.started = p.now()
	gr.gate = gate.Run(ctx, gopts)
	// Record the gate even when ctx is done, so an interrupted gate blocks the task as
	// interrupted instead of failing it on the event write.
	if _, err := p.Store.AppendEvent(context.WithoutCancel(ctx), "task-gate", "", t.ID, map[string]any{"attempt": n, "head": head, "result": gr.gate}); err != nil {
		return gr, err
	}
	if ctx.Err() != nil || gr.gate.Passed || len(gr.gate.Commands) == 0 {
		return gr, nil
	}
	gr.triage, err = p.triage(ctx, t, n, startSHA, gopts, gr.gate)
	return gr, err
}

// reusedGate returns the stored passing gate for tree and fp, if there is one, and records it
// as the attempt's gate result marked reused. ok is false when the gate must run.
func (p *Pipeline) reusedGate(ctx context.Context, t store.Task, n int, head, tree, fp string) (g gate.Result, ok bool, err error) {
	prev, ok, err := p.Store.PassedGate(ctx, tree, fp)
	if err != nil || !ok {
		return g, false, err
	}
	if err := json.Unmarshal(prev.Result, &g); err != nil {
		return g, false, fmt.Errorf("the stored gate result of tree %s is unreadable: %w", tree, err)
	}
	if !g.Passed {
		return g, false, fmt.Errorf("the stored gate result of tree %s is not a pass", tree)
	}
	if err := p.Store.SetTaskStatus(ctx, t.ID, store.TaskRunning, store.PhaseGate, ""); err != nil {
		return g, false, err
	}
	if _, err := p.Store.AppendEvent(ctx, "task-gate", "", t.ID, map[string]any{"attempt": n, "head": head, "result": g, "reused": true, "tree": tree, "reused_from": prev.TaskID}); err != nil {
		return g, false, err
	}
	p.logf("%s a%d gate reused (tree %s)", t.ID, n, tree)
	return g, true, nil
}

// recordGatePass stores a passing gate result for reuse on the same tree.
func (p *Pipeline) recordGatePass(ctx context.Context, t store.Task, tree, fp string, g gate.Result) error {
	res, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode the gate result of %s: %w", t.ID, err)
	}
	return p.Store.RecordGatePass(ctx, tree, fp, t.ID, res)
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
		// From here on this call owns the branch and worktree; remove them if preparing fails
		// before the branch is recorded.
		fail := func(err error) (store.Branch, error) {
			return br, errors.Join(err, p.removeCreated(ctx, wt, name))
		}
		for _, m := range merges {
			// On failure mergeDependency has already removed the worktree and branch, so a
			// retry starts clean.
			if err := p.mergeDependency(ctx, t, wt, name, m); err != nil {
				return br, err
			}
		}
		startSHA, err := gitx.RevParse(ctx, wt, "HEAD")
		if err != nil {
			return fail(err)
		}
		br = store.Branch{TaskID: t.ID, Name: name, Base: base, BaseSHA: baseSHA, StartSHA: startSHA, HeadSHA: startSHA, Worktree: wt}
		if err := p.Store.SaveBranch(ctx, br); err != nil {
			return fail(err)
		}
		return br, nil
	default:
		return br, err
	}
	return br, p.Store.SaveBranch(ctx, br)
}

// removeCreated removes a worktree and branch this call just created. The worktree holds only
// the starting point, so it is removed with force.
func (p *Pipeline) removeCreated(ctx context.Context, wt, name string) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	if err := gitx.RemoveWorktree(ctx, p.Paths.Root, wt); err != nil {
		errs = append(errs, fmt.Errorf("removing worktree %s failed: %w", wt, err))
	}
	if err := gitx.DeleteBranch(ctx, p.Paths.Root, name); err != nil {
		errs = append(errs, fmt.Errorf("deleting branch %s failed: %w", name, err))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("cleanup after failed preparation left branch %s and worktree %s behind; remove them by hand (git worktree remove --force, git branch -D): %w", name, wt, errors.Join(errs...))
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
// model is the model the agent runs on and modelWhy why it was chosen; both are recorded on
// the attempt.
func (p *Pipeline) agent(ctx context.Context, t store.Task, n int, role runner.Role, model, modelWhy, promptText string,
	schema json.RawMessage, tools []string, limits budget.Limits, dir, logDir string) (agentRun, error) {
	id, err := p.Store.StartAttempt(ctx, t.ID, n, string(role))
	if err != nil {
		return agentRun{}, err
	}
	if err := p.Store.SetAttemptModel(ctx, id, model, modelWhy); err != nil {
		// The attempt never ran: close it so it does not stay running.
		ferr := p.Store.FinishAttempt(context.WithoutCancel(ctx), id, "error", budget.Usage{}, false, "", "")
		return agentRun{}, errors.Join(err, ferr)
	}
	rctx, cancel := context.WithTimeout(ctx, p.Cfg.Runner.AttemptTimeout.Duration)
	defer cancel()

	var mu sync.Mutex
	var spent budget.Usage
	var over string
	var startErr error
	started := p.now()
	live := p.startLiveUsage(ctx, id, t.ID, model)
	defer live.stop() // also on a runner panic; the normal path stops it explicitly below
	res, runErr := p.Runner.Run(rctx, runner.Request{
		Role: role, TaskID: t.ID, Prompt: promptText, Dir: dir, Schema: schema, Tools: tools,
		Model: model, Budget: limits, Count: p.Cfg.Budget.Count,
		Name:   fmt.Sprintf("%s a%d %s", t.ID, n, role),
		LogDir: logDir, ProtectedPaths: []string{p.Paths.MillDir}, Env: p.env(p.AgentEnvAllow),
		TmpDir: filepath.Join(p.Cfg.Worktrees(p.Paths.Root, p.Home), ".tmp", t.ID, fmt.Sprintf("a%d-%s", n, role)),
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
	if err := p.Store.FinishAttempt(context.WithoutCancel(ctx), id, status, res.Usage, res.CostEstimated, outPath, res.RawPath, res.Duration); err != nil {
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

// DefaultLiveUsageInterval is the minimum time between live usage writes for one attempt
// when Pipeline.LiveUsageInterval is zero.
const DefaultLiveUsageInterval = 2 * time.Second

func (p *Pipeline) liveUsageInterval() time.Duration {
	if p.LiveUsageInterval > 0 {
		return p.LiveUsageInterval
	}
	return DefaultLiveUsageInterval
}

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
	interval := p.liveUsageInterval()
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
				case <-time.After(interval):
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

// budgetStep is the step a budget check reserves for.
type budgetStep int

const (
	stepImplement budgetStep = iota // an agent that may use a whole attempt
	stepVerify                      // the verifier alone, which may use its share of an attempt
)

// reserve returns what the budgets must still cover before step runs for a task of the given
// size, and how a block reason says that they cannot.
func (p *Pipeline) reserve(step budgetStep, size string) (need budget.Limits, what, user string) {
	if step == stepVerify {
		return p.Cfg.VerifierBudget(size), "the verifier", "the verifier"
	}
	return p.Cfg.AttemptBudget(size), "another attempt", "an attempt"
}

// checkBudgets blocks when the task budget, or for a goal task the goal budget, cannot cover
// step (BG2).
func (p *Pipeline) checkBudgets(ctx context.Context, t store.Task, step budgetStep) (bool, string, error) {
	if blocked, reason, err := p.checkTaskBudget(ctx, t, step); err != nil || blocked {
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
	need, _, user := p.reserve(step, t.Size)
	if !budget.Covers(p.Cfg.Budget.Goal, spent, need, p.Cfg.Budget.Count) {
		return true, fmt.Sprintf("goal budget exhausted for %s: spent %s of %s, %s may use %s",
			goal, fmtUsage(spent), fmtLimits(p.Cfg.Budget.Goal), user, fmtLimits(need)), nil
	}
	return false, "", nil
}

func (p *Pipeline) checkTaskBudget(ctx context.Context, t store.Task, step budgetStep) (bool, string, error) {
	spent, err := p.Store.TaskUsage(ctx, t.ID)
	if err != nil {
		return false, "", err
	}
	need, what, user := p.reserve(step, t.Size)
	if !budget.Covers(p.Cfg.Budget.Task, spent, need, p.Cfg.Budget.Count) {
		return true, fmt.Sprintf("task budget cannot cover %s: spent %s of %s, %s may use %s",
			what, fmtUsage(spent), fmtLimits(p.Cfg.Budget.Task), user, fmtLimits(need)), nil
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

// siblings lists the other tasks of t's goal for the verifier prompt; nil for a standalone task.
func (p *Pipeline) siblings(ctx context.Context, t store.Task) ([]prompt.SiblingTask, error) {
	goal, _ := store.SplitTaskID(t.ID)
	if goal == "" {
		return nil, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("list the tasks of goal %s for the verifier of %s: %w", goal, t.ID, err)
	}
	var out []prompt.SiblingTask
	for _, s := range tasks {
		if s.ID == t.ID {
			continue
		}
		out = append(out, prompt.SiblingTask{ID: s.ID, Title: s.Title, Owns: s.Owns, Summary: specSummary(s.Spec.Spec)})
	}
	return out, nil
}

// specSummary is the first sentence of a spec, cut to at most 120 characters.
func specSummary(spec string) string {
	s := strings.Join(strings.Fields(spec), " ")
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return s
}
