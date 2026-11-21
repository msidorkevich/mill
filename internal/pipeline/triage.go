package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mill/internal/gate"
	"mill/internal/gitx"
	"mill/internal/prompt"
	"mill/internal/store"
)

// triageResult says what a failed gate means after triage.
type triageResult struct {
	gate     gate.Result      // the gate with re-run results folded in
	flaky    []string         // commands that failed once and passed on the re-run
	blocked  string           // non-empty: every remaining failure also fails on the start point
	findings []prompt.Finding // when the gate still fails (recorded for `task show` when blocked)
}

// triage decides what a failed gate means: a flaky command passes on a re-run (gate_rerun);
// a command that also fails on the task's starting point is not the task's fault
// (gate_check_base). The caller checks ctx afterwards: an interrupted triage is meaningless.
func (p *Pipeline) triage(ctx context.Context, t store.Task, n int, startSHA string, opts gate.Options, g gate.Result) (triageResult, error) {
	tr := triageResult{gate: g}
	var failed []int // indexes into tr.gate.Commands
	for i, c := range g.Commands {
		if !c.Passed() {
			failed = append(failed, i)
		}
	}
	if len(failed) == 0 {
		tr.findings = gateFindings(g)
		return tr, nil
	}

	if p.Cfg.GateRerun {
		ro := opts
		ro.Cmds = cmdsAt(g, failed)
		ro.LogDir = filepath.Join(opts.LogDir, "rerun")
		rr := gate.Run(ctx, ro)
		if ctx.Err() != nil {
			return tr, nil
		}
		var still []int
		tr.gate.Commands = append([]gate.CmdResult(nil), g.Commands...)
		for k, i := range failed {
			if k < len(rr.Commands) && rr.Commands[k].Passed() {
				tr.flaky = append(tr.flaky, g.Commands[i].Cmd)
				tr.gate.Commands[i] = rr.Commands[k]
				continue
			}
			still = append(still, i)
		}
		if len(tr.flaky) > 0 {
			if _, err := p.Store.AppendEvent(ctx, "gate-flaky", "", t.ID, map[string]any{"attempt": n, "commands": tr.flaky}); err != nil {
				return tr, err
			}
			p.logf("%s a%d gate      flaky (passed on a re-run): %s", t.ID, n, strings.Join(tr.flaky, "; "))
		}
		if len(still) == 0 {
			tr.gate.Passed, tr.gate.Reason = true, ""
			return tr, nil
		}
		failed = still
		tr.gate.Reason = "failed: " + strings.Join(cmdsAt(tr.gate, failed), "; ")
	}

	if !p.Cfg.GateCheckBase {
		tr.findings = gateFindings(tr.gate)
		return tr, nil
	}
	base, err := p.runOnBase(ctx, t, startSHA, opts, cmdsAt(tr.gate, failed))
	if ctx.Err() != nil {
		return tr, nil
	}
	if err != nil {
		p.logf("%s a%d gate      warning: could not check the failing commands on the starting point %s, using the gate findings as they are: %v", t.ID, n, short(startSHA), err)
		tr.findings = gateFindings(tr.gate)
		return tr, nil
	}

	var causedHere, alsoOnBase []string
	for k, i := range failed {
		c := tr.gate.Commands[i]
		if base.Commands[k].Passed() {
			causedHere = append(causedHere, c.Cmd)
			f := cmdFinding(c)
			f.Issue += fmt.Sprintf("\nIt passes on the task's starting point %s, so the task's changes cause it.", short(startSHA))
			tr.findings = append(tr.findings, f)
		} else {
			alsoOnBase = append(alsoOnBase, c.Cmd)
		}
	}
	for _, c := range alsoOnBase {
		tr.findings = append(tr.findings, prompt.Finding{Source: "gate", Issue: fmt.Sprintf(
			"`%s` fails on the task's starting point %s too, so it is not caused by this task; do not try to fix it", c, short(startSHA))})
	}
	if len(causedHere) == 0 {
		if _, err := p.Store.AppendEvent(ctx, "gate-base-failing", "", t.ID, map[string]any{"attempt": n, "start": startSHA, "commands": alsoOnBase}); err != nil {
			return tr, err
		}
		tr.blocked = fmt.Sprintf("gate command(s) %s fail on the task's starting point %s too, so this task did not cause them; fix them on the base branch, then retry",
			quoteCmds(alsoOnBase), short(startSHA))
	}
	return tr, nil
}

// runOnBase runs cmds in a temporary detached worktree at startSHA, with the gate's
// environment and timeout, logging under <logDir>/base. The worktree is always removed. An
// error means the commands could not be run there, so their results say nothing.
func (p *Pipeline) runOnBase(ctx context.Context, t store.Task, startSHA string, opts gate.Options, cmds []string) (res gate.Result, err error) {
	if startSHA == "" {
		return res, errors.New("the task has no recorded starting point")
	}
	parent := filepath.Join(p.Cfg.Worktrees(p.Paths.Root, p.Home), ".base", t.ID)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp(parent, "wt-")
	if err != nil {
		return res, err
	}
	p.gitMu.Lock()
	addErr := gitx.AddDetachedWorktree(ctx, p.Paths.Root, dir, startSHA)
	p.gitMu.Unlock()
	if addErr != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return res, fmt.Errorf("create base worktree: %w; and removing %s failed: %v", addErr, dir, rmErr)
		}
		return res, fmt.Errorf("create base worktree: %w", addErr)
	}
	defer func() {
		p.gitMu.Lock()
		defer p.gitMu.Unlock()
		// Remove even when ctx is done, so nothing is left behind.
		if rmErr := gitx.RemoveWorktree(context.WithoutCancel(ctx), p.Paths.Root, dir); rmErr != nil {
			p.logf("%s: warning: removing the temporary base worktree failed: %v; run `git worktree remove --force %s`", t.ID, rmErr, dir)
		}
	}()

	bo := opts
	bo.Dir = dir
	bo.Cmds = cmds
	bo.LogDir = filepath.Join(opts.LogDir, "base")
	res = gate.Run(ctx, bo)
	if len(res.Commands) != len(cmds) {
		return res, fmt.Errorf("ran %d of %d command(s) on the starting point: %s", len(res.Commands), len(cmds), res.Reason)
	}
	for _, c := range res.Commands {
		if strings.HasPrefix(c.Error, "could not run") {
			return res, fmt.Errorf("`%s` on the starting point: %s", c.Cmd, c.Error)
		}
	}
	return res, nil
}

func cmdsAt(g gate.Result, idx []int) []string {
	out := make([]string, len(idx))
	for k, i := range idx {
		out[k] = g.Commands[i].Cmd
	}
	return out
}

func quoteCmds(cmds []string) string {
	q := make([]string, len(cmds))
	for i, c := range cmds {
		q[i] = "`" + c + "`"
	}
	return strings.Join(q, ", ")
}
