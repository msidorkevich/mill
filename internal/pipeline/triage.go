package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// triageResult says what a failed gate means after triage.
type triageResult struct {
	gate     gate.Result      // the gate with re-run results folded in
	flaky    []string         // commands that failed once and passed on the re-run
	blocked  string           // non-empty: every remaining failure also fails on the start point
	findings []prompt.Finding // when the gate still fails (recorded for `task show` when blocked)
	// realTests holds, per index into gate.Commands, the Go tests that failed in two of three
	// runs of a command whose first two runs failed on different tests. Only they are findings.
	realTests map[int][]string
}

// triage decides what a failed gate means: a flaky command passes on a re-run (gate_rerun);
// a command that also fails on the task's starting point is not the task's fault
// (gate_check_base). A command no host could start never ran: it is neither re-run nor
// checked on the starting point, and never counted as flaky. It runs inside the gate slot
// the caller holds for the gate, so its re-runs take turns with other gates too. The caller
// checks ctx afterwards: an interrupted triage is meaningless.
func (p *Pipeline) triage(ctx context.Context, t store.Task, n int, startSHA string, opts gate.Options, g gate.Result) (triageResult, error) {
	tr := triageResult{gate: g}
	var failed []int    // indexes into tr.gate.Commands of commands that ran and failed
	var unstarted []int // indexes of commands no host could start
	for i, c := range g.Commands {
		switch {
		case c.Passed():
		case c.StartFailed:
			unstarted = append(unstarted, i)
		default:
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
		var disjoint []int                  // commands whose two runs failed on different Go tests
		seen := map[int][]string{}          // failing tests of the earlier runs, per disjoint command
		flakyTests := map[string][]string{} // flaky tests, per command found flaky by its tests
		tr.gate.Commands = append([]gate.CmdResult(nil), g.Commands...)
		for k, i := range failed {
			if k < len(rr.Commands) && rr.Commands[k].Passed() {
				tr.flaky = append(tr.flaky, g.Commands[i].Cmd)
				tr.gate.Commands[i] = rr.Commands[k]
				continue
			}
			if k < len(rr.Commands) {
				if tests, ok := disjointGoTests(g.Commands[i].LogPath, rr.Commands[k].LogPath); ok {
					disjoint = append(disjoint, i)
					seen[i] = tests
					continue
				}
			}
			still = append(still, i)
		}
		if len(disjoint) > 0 {
			// Two runs failing on different tests say nothing yet: a third run decides.
			ro.Cmds = cmdsAt(g, disjoint)
			ro.LogDir = filepath.Join(opts.LogDir, "rerun2")
			r3 := gate.Run(ctx, ro)
			if ctx.Err() != nil {
				return tr, nil
			}
			for k, i := range disjoint {
				if k >= len(r3.Commands) {
					still = append(still, i)
					continue
				}
				c3 := r3.Commands[k]
				if c3.Passed() {
					tr.flaky = append(tr.flaky, c3.Cmd)
					flakyTests[c3.Cmd] = seen[i]
					tr.gate.Commands[i] = c3
					continue
				}
				third, ok, err := gate.GoTestFailuresFromLog(c3.LogPath)
				if err != nil || !ok {
					// The check cannot decide: the command's first failure stands.
					still = append(still, i)
					continue
				}
				tr.gate.Commands[i] = c3
				var real []string // tests failing in two of the three runs
				for _, x := range third {
					if slices.Contains(seen[i], x) {
						real = append(real, x)
					}
				}
				if len(real) == 0 {
					tr.flaky = append(tr.flaky, c3.Cmd)
					all := append(slices.Clone(seen[i]), third...)
					slices.Sort(all)
					flakyTests[c3.Cmd] = slices.Compact(all)
					continue
				}
				if tr.realTests == nil {
					tr.realTests = map[int][]string{}
				}
				tr.realTests[i] = real
				still = append(still, i)
			}
			slices.Sort(still)
		}
		if len(tr.flaky) > 0 {
			data := map[string]any{"attempt": n, "commands": tr.flaky}
			if len(flakyTests) > 0 {
				data["tests"] = flakyTests
			}
			if _, err := p.Store.AppendEvent(ctx, "gate-flaky", "", t.ID, data); err != nil {
				return tr, err
			}
			p.logf("%s a%d gate      flaky (passed on a re-run): %s", t.ID, n, strings.Join(tr.flaky, "; "))
			for _, c := range tr.flaky {
				if tests, ok := flakyTests[c]; ok {
					p.logf("%s a%d gate      flaky tests of `%s` (each failed in one run only): %s", t.ID, n, c, strings.Join(tests, ", "))
				}
			}
		}
		if len(still) == 0 && len(unstarted) == 0 {
			tr.gate.Passed, tr.gate.Reason = true, ""
			return tr, nil
		}
		failed = still
		tr.gate.Reason = "failed: " + strings.Join(cmdsAt(tr.gate, slices.Sorted(slices.Values(append(slices.Clone(failed), unstarted...)))), "; ")
		if len(failed) == 0 {
			tr.findings = gateFindings(tr.gate)
			return tr, nil
		}
	}

	if !p.Cfg.GateCheckBase {
		tr.findings = tr.gateFindings()
		return tr, nil
	}
	base, err := p.runOnBase(ctx, t, startSHA, opts, cmdsAt(tr.gate, failed))
	if ctx.Err() != nil {
		return tr, nil
	}
	if err != nil {
		p.logf("%s a%d gate      warning: could not check the failing commands on the starting point %s, using the gate findings as they are: %v", t.ID, n, short(startSHA), err)
		tr.findings = tr.gateFindings()
		return tr, nil
	}

	var causedHere, alsoOnBase []string
	for _, i := range unstarted {
		// It never ran, so it is the attempt's failure: the starting point cannot explain it.
		causedHere = append(causedHere, tr.gate.Commands[i].Cmd)
		tr.findings = append(tr.findings, cmdFinding(tr.gate.Commands[i]))
	}
	tests := map[string]any{} // per command compared by failing tests: head, base, head_only
	for k, i := range failed {
		c := tr.gate.Commands[i]
		b := base.Commands[k]
		real, restricted := tr.realTests[i]
		if b.Passed() {
			causedHere = append(causedHere, c.Cmd)
			f := cmdFinding(c)
			if restricted {
				f = realTestFinding(c, real)
			}
			f.Issue += fmt.Sprintf("\nIt passes on the task's starting point %s, so the task's changes cause it.", short(startSHA))
			tr.findings = append(tr.findings, f)
			continue
		}
		cmp, ok := compareGoTests(c.LogPath, b.LogPath)
		if !ok {
			alsoOnBase = append(alsoOnBase, c.Cmd)
			continue
		}
		if restricted {
			cmp = restrictHead(cmp, real)
		}
		tests[c.Cmd] = map[string]any{"head": cmp.head, "base": cmp.base, "head_only": cmp.headOnly}
		if len(cmp.headOnly) == 0 {
			alsoOnBase = append(alsoOnBase, c.Cmd)
			continue
		}
		causedHere = append(causedHere, c.Cmd)
		tr.findings = append(tr.findings, goTestFinding(c, cmp, startSHA))
	}
	for _, c := range alsoOnBase {
		tr.findings = append(tr.findings, prompt.Finding{Source: "gate", Issue: fmt.Sprintf(
			"`%s` fails on the task's starting point %s too, so it is not caused by this task; do not try to fix it", c, short(startSHA))})
	}
	if len(causedHere) == 0 {
		data := map[string]any{"attempt": n, "start": startSHA, "commands": alsoOnBase}
		if len(tests) > 0 {
			data["tests"] = tests
		}
		if _, err := p.Store.AppendEvent(ctx, "gate-base-failing", "", t.ID, data); err != nil {
			return tr, err
		}
		tr.blocked = fmt.Sprintf("gate command(s) %s fail on the task's starting point %s too, so this task did not cause them; fix them on the base branch, run `mill task rebase %s`, then retry",
			quoteCmds(alsoOnBase), short(startSHA), t.ID)
	} else if len(tests) > 0 {
		if _, err := p.Store.AppendEvent(ctx, "gate-base-compared", "", t.ID, map[string]any{"attempt": n, "start": startSHA, "tests": tests}); err != nil {
			return tr, err
		}
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
		if c.StartFailed || strings.HasPrefix(c.Error, "could not run") {
			return res, fmt.Errorf("`%s` on the starting point: %s", c.Cmd, c.Error)
		}
	}
	return res, nil
}

// goTestExcerptLines caps the log excerpt in a finding about tests failing only on the head.
const goTestExcerptLines = 60

// goTestComparison is the failing tests of one gate command on the task's head and on its
// starting point.
type goTestComparison struct {
	head, base         []string
	headOnly, baseOnly []string
	shared             []string
}

// compareGoTests reads the failing tests from the head and base logs of one command. ok is
// false when either log cannot be read or is not go test output; the caller then compares
// the whole command.
func compareGoTests(headLog, baseLog string) (goTestComparison, bool) {
	head, hok, herr := gate.GoTestFailuresFromLog(headLog)
	base, bok, berr := gate.GoTestFailuresFromLog(baseLog)
	if herr != nil || berr != nil || !hok || !bok {
		return goTestComparison{}, false
	}
	cmp := goTestComparison{head: head, base: base, headOnly: []string{}, baseOnly: []string{}, shared: []string{}}
	for _, h := range head {
		if slices.Contains(base, h) {
			cmp.shared = append(cmp.shared, h)
		} else {
			cmp.headOnly = append(cmp.headOnly, h)
		}
	}
	for _, b := range base {
		if !slices.Contains(head, b) {
			cmp.baseOnly = append(cmp.baseOnly, b)
		}
	}
	return cmp, true
}

// goTestFinding describes a command whose head-only failing tests the task caused, with an
// excerpt of their output and the tests that fail on the starting point too.
func goTestFinding(c gate.CmdResult, cmp goTestComparison, startSHA string) prompt.Finding {
	issue := fmt.Sprintf("`%s` failed with these tests failing only on the task's head: %s. They pass on the task's starting point %s, so the task's changes cause them.",
		c.Cmd, strings.Join(cmp.headOnly, ", "), short(startSHA))
	excerpt := ""
	if b, err := os.ReadFile(c.LogPath); err == nil {
		excerpt = gate.GoTestExcerpt(string(b), cmp.headOnly, goTestExcerptLines)
	}
	if excerpt != "" {
		issue += " Output of those tests:\n" + indent(excerpt)
	} else if c.Tail != "" {
		issue += " Output (last lines):\n" + indent(c.Tail)
	}
	if pre := append(append([]string(nil), cmp.shared...), cmp.baseOnly...); len(pre) > 0 {
		slices.Sort(pre)
		issue += fmt.Sprintf("\nAlso failing on the starting point %s, not your fault; do not try to fix them: %s.", short(startSHA), strings.Join(pre, ", "))
	}
	return prompt.Finding{Source: "gate", Issue: issue}
}

// disjointGoTests reads the failing Go tests of two failed runs of one command. ok is true
// only when both logs are go test output and no test failed in both runs; tests is then the
// failing tests of both runs, sorted.
func disjointGoTests(firstLog, secondLog string) (tests []string, ok bool) {
	cmp, ok := compareGoTests(secondLog, firstLog)
	if !ok || len(cmp.shared) > 0 {
		return nil, false
	}
	tests = append(slices.Clone(cmp.head), cmp.base...)
	slices.Sort(tests)
	return tests, true
}

// restrictHead narrows a head/base comparison to the head's real failing tests.
func restrictHead(cmp goTestComparison, real []string) goTestComparison {
	out := goTestComparison{head: real, base: cmp.base, headOnly: []string{}, baseOnly: []string{}, shared: []string{}}
	for _, h := range real {
		if slices.Contains(cmp.base, h) {
			out.shared = append(out.shared, h)
		} else {
			out.headOnly = append(out.headOnly, h)
		}
	}
	for _, b := range cmp.base {
		if !slices.Contains(real, b) {
			out.baseOnly = append(out.baseOnly, b)
		}
	}
	return out
}

// gateFindings is the findings of the triaged gate's failing commands; a command with real
// failing tests names only them.
func (tr triageResult) gateFindings() []prompt.Finding {
	if len(tr.gate.Commands) == 0 {
		return gateFindings(tr.gate)
	}
	var f []prompt.Finding
	for i, c := range tr.gate.Commands {
		if c.Passed() {
			continue
		}
		if real, ok := tr.realTests[i]; ok {
			f = append(f, realTestFinding(c, real))
			continue
		}
		f = append(f, cmdFinding(c))
	}
	return f
}

// realTestFinding describes a command whose first two runs failed on different Go tests and
// whose third run failed some of them again: only those tests are the command's failures.
func realTestFinding(c gate.CmdResult, real []string) prompt.Finding {
	issue := fmt.Sprintf("`%s` failed with these tests failing in two of three runs: %s.", c.Cmd, strings.Join(real, ", "))
	excerpt := ""
	if b, err := os.ReadFile(c.LogPath); err == nil {
		excerpt = gate.GoTestExcerpt(string(b), real, goTestExcerptLines)
	}
	if excerpt != "" {
		issue += " Output of those tests:\n" + indent(excerpt)
	} else if c.Tail != "" {
		issue += " Output (last lines):\n" + indent(c.Tail)
	}
	return prompt.Finding{Source: "gate", Issue: issue}
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
