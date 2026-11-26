package keeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
)

// CombinedResult is what one combined check found.
type CombinedResult struct {
	Ran      bool     `json:"ran"`      // false when fewer than two branches were ready
	Passed   bool     `json:"passed"`   // every ready branch merged together passes the gate
	Branches []string `json:"branches"` // the ready tasks, in merge order
	Culprit  string   `json:"culprit,omitempty"`
	Partner  string   `json:"partner,omitempty"` // empty when the culprit is red with the base alone
	Failed   string   `json:"failed,omitempty"`  // what failed for the culprit (and its partner)
	Probes   int      `json:"probes"`            // gate runs on merged states
	Cached   bool     `json:"cached"`            // nothing changed since the last check; this is its result
}

// readyBranch is a verified or published task whose gate ran at its head.
type readyBranch struct {
	task store.Task
	br   store.Branch
}

// combined holds the state of one combined check.
type combined struct {
	Options
	dir    string // the throwaway worktree
	base   string // the remote base branch tip
	logDir string
	host   host.Host
	env    []string
	res    *CombinedResult
}

// probeResult is the outcome of the gate on one merged state.
type probeResult struct {
	red    bool
	failed string
}

// Combined merges every ready branch onto the remote base branch in a throwaway worktree and
// runs the gate there. When it is red it bisects the merge order for the culprit and looks for
// the earlier branch it conflicts with; such a pair is recorded as a combined-conflict event
// and in both tasks' reasons, without blocking either. A gate that cannot run is an error.
func Combined(ctx context.Context, o Options) (res CombinedResult, err error) {
	res = CombinedResult{Branches: []string{}}
	ready, err := readyBranches(ctx, o)
	if err != nil {
		return res, err
	}
	for _, r := range ready {
		res.Branches = append(res.Branches, r.task.ID)
	}
	if len(ready) < 2 {
		return res, nil
	}
	claimer := o.Claimer
	if claimer.Owner == "" {
		claimer = store.NewClaimer("keep")
	}
	claimed, err := claimReady(ctx, o, claimer, ready)
	defer func() {
		if rerr := o.Store.ReleaseClaims(context.WithoutCancel(ctx), claimer, claimed); rerr != nil {
			err = errors.Join(err, fmt.Errorf("combined check: %w", rerr))
		}
	}()
	if err != nil {
		return res, err
	}
	// A branch may have changed before it was claimed: read the ready set again.
	if ready, err = readyBranches(ctx, o); err != nil {
		return res, err
	}
	ready = slices.DeleteFunc(ready, func(r readyBranch) bool { return !slices.Contains(claimed, r.task.ID) })
	res.Branches = []string{}
	for _, r := range ready {
		res.Branches = append(res.Branches, r.task.ID)
	}
	if len(ready) < 2 {
		return res, nil
	}
	res.Ran = true

	upstream := o.Cfg.Remote + "/" + o.Cfg.BaseBranch
	base, err := gitx.RevParse(ctx, o.Paths.Root, "refs/remotes/"+upstream)
	if err != nil {
		return res, fmt.Errorf("combined check: remote %s has no branch %s: %w", o.Cfg.Remote, o.Cfg.BaseBranch, err)
	}
	fp := fingerprint(base, ready)
	if !o.Force {
		cached, ok, err := cachedCombined(ctx, o, fp)
		if err != nil {
			return res, err
		}
		if ok {
			cached.Branches = res.Branches
			verdict := "green"
			if !cached.Passed {
				verdict = "red: " + cached.Culprit + "/" + cached.Partner
			}
			if o.Out != nil {
				fmt.Fprintf(o.Out, "combined check: unchanged since the last check (%s)\n", verdict)
			}
			return cached, nil
		}
	}
	defer func() {
		if err != nil {
			return
		}
		result := "green"
		if !res.Passed {
			result = "red"
		}
		if _, aerr := o.Store.AppendEvent(ctx, "combined-checked", "", "", map[string]any{
			"fingerprint": fp, "base": base, "branches": res.Branches, "result": result,
			"culprit": res.Culprit, "partner": res.Partner, "failed": res.Failed}); aerr != nil {
			err = fmt.Errorf("record the combined check result: %w", aerr)
		}
	}()
	home := ""
	if o.Pipeline != nil {
		home = o.Pipeline.Home
	}
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return res, fmt.Errorf("combined check: no home directory for the default worktree root; set worktree_root: %w", err)
		}
	}
	wtRoot := o.Cfg.Worktrees(o.Paths.Root, home)
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return res, fmt.Errorf("combined check: create the worktree root %s: %w", wtRoot, err)
	}
	dir, err := os.MkdirTemp(wtRoot, ".combined-")
	if err != nil {
		return res, fmt.Errorf("combined check: create a throwaway worktree under %s: %w", wtRoot, err)
	}
	defer func() {
		if cerr := removeThrowaway(context.WithoutCancel(ctx), o.Paths.Root, dir); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	if err := gitx.AddDetachedWorktree(ctx, o.Paths.Root, dir, base); err != nil {
		return res, fmt.Errorf("combined check: add a throwaway worktree at %s: %w", dir, err)
	}

	h := o.Host
	if h == nil {
		h = host.NewExec()
	}
	c := &combined{
		Options: o, dir: dir, base: base, host: h, res: &res,
		logDir: filepath.Join(o.Paths.Runs, "combined", filepath.Base(dir)),
		env:    host.AllowEnv(os.Environ(), append(append([]string{}, host.DefaultEnvAllow...), o.Cfg.EnvAllow...)),
	}
	c.logf("combined check: merging %d ready branches onto %s (%s)", len(ready), upstream, short(base))

	all, err := c.probe(ctx, ready)
	if err != nil {
		return res, err
	}
	if !all.red {
		res.Passed = true
		c.logf("combined check: green")
		return res, nil
	}

	// The shortest red prefix: prefixes shorter than lo are green, the prefix of hi is red.
	failures := map[int]probeResult{len(ready): all}
	lo, hi := 1, len(ready)
	for lo < hi {
		mid := (lo + hi) / 2
		p, err := c.probe(ctx, ready[:mid])
		if err != nil {
			return res, err
		}
		if p.red {
			failures[mid] = p
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	culprit := ready[hi-1]
	res.Culprit = culprit.task.ID

	// A culprit red with the base alone has no partner: every pair with it would be red.
	alone := failures[1]
	if hi > 1 {
		if alone, err = c.probe(ctx, []readyBranch{culprit}); err != nil {
			return res, err
		}
	}
	if alone.red {
		res.Failed = alone.failed
		c.logf("combined check: %s is red with the base alone:\n%s", culprit.task.ID, alone.failed)
		return res, nil
	}

	for _, p := range ready[:hi-1] {
		pr, err := c.probe(ctx, []readyBranch{p, culprit})
		if err != nil {
			return res, err
		}
		if pr.red {
			res.Partner, res.Failed = p.task.ID, pr.failed
			return res, c.recordPair(ctx, culprit.task, p.task, pr.failed)
		}
	}

	var earlier []string
	for _, p := range ready[:hi-1] {
		earlier = append(earlier, p.task.ID)
	}
	res.Failed = fmt.Sprintf("red only together with several of %s, never with one of them alone:\n%s",
		strings.Join(earlier, ", "), failures[hi].failed)
	c.logf("combined check: %s is %s", culprit.task.ID, res.Failed)
	return res, nil
}

// claimReady claims each ready branch's task for c, so that nothing changes it during the
// check, and records "<command>: combined check" as what c is doing with it. A busy task is
// left out of the check. claimed lists the tasks c holds, also when err is set.
func claimReady(ctx context.Context, o Options, c store.Claimer, ready []readyBranch) (claimed []string, err error) {
	for _, r := range ready {
		id := r.task.ID
		err := o.Store.ClaimTasks(ctx, c, []string{id})
		var busy *store.BusyError
		if errors.As(err, &busy) {
			if o.Out != nil {
				fmt.Fprintf(o.Out, "combined check: %s is busy (%s); left out\n", id, busy.Doing)
			}
			continue
		}
		if err != nil {
			return claimed, fmt.Errorf("combined check: %w", err)
		}
		claimed = append(claimed, id)
		if err := o.Store.SetClaimActivity(ctx, c, id, c.Command+": combined check"); err != nil {
			return claimed, fmt.Errorf("combined check: %s: record step %q on its claim: %w", id, "combined check", err)
		}
	}
	return claimed, nil
}

// fingerprint identifies what a combined check covers: the base tip and the ready branch heads.
func fingerprint(base string, ready []readyBranch) string {
	parts := make([]string, 0, len(ready))
	for _, r := range ready {
		parts = append(parts, r.task.ID+"@"+r.br.HeadSHA)
	}
	sort.Strings(parts)
	return base + " " + strings.Join(parts, " ")
}

// cachedCombined returns the result of the latest combined check when it covered fp.
func cachedCombined(ctx context.Context, o Options, fp string) (CombinedResult, bool, error) {
	ev, ok, err := o.Store.LatestEvent(ctx, "combined-checked")
	if err != nil || !ok {
		return CombinedResult{}, false, err
	}
	var d struct {
		Fingerprint string `json:"fingerprint"`
		Result      string `json:"result"`
		Culprit     string `json:"culprit"`
		Partner     string `json:"partner"`
		Failed      string `json:"failed"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return CombinedResult{}, false, fmt.Errorf("combined-checked event %d is unreadable: %w", ev.Seq, err)
	}
	if d.Fingerprint != fp {
		return CombinedResult{}, false, nil
	}
	return CombinedResult{Ran: true, Passed: d.Result == "green", Culprit: d.Culprit, Partner: d.Partner,
		Failed: d.Failed, Cached: true}, true, nil
}

func (c *combined) logf(format string, a ...any) {
	if c.Out != nil {
		fmt.Fprintf(c.Out, format+"\n", a...)
	}
}

// probe rebuilds the throwaway worktree as the base tip with branches merged in order and runs
// check_cmd plus each merged task's checks. A merge conflict is red. An error means the state
// could not be built or the gate could not run.
func (c *combined) probe(ctx context.Context, branches []readyBranch) (probeResult, error) {
	if _, err := gitx.Run(ctx, c.dir, "reset", "-q", "--hard", c.base); err != nil {
		return probeResult{}, fmt.Errorf("combined check: reset the throwaway worktree to %s: %w", short(c.base), err)
	}
	if _, err := gitx.Run(ctx, c.dir, "clean", "-q", "-ffdx"); err != nil {
		return probeResult{}, fmt.Errorf("combined check: clean the throwaway worktree: %w", err)
	}
	c.res.Probes++
	var ids []string
	cmds := []string{c.Cfg.CheckCmd}
	for _, b := range branches {
		ids = append(ids, b.task.ID)
		err := gitx.Merge(ctx, c.dir, b.br.HeadSHA, fmt.Sprintf("mill: combined check merges %s (%s)", b.br.Name, b.task.ID))
		var ce *gitx.ConflictError
		if errors.As(err, &ce) {
			failed := fmt.Sprintf("  merge conflict: %s (%s) conflicts in %s", b.task.ID, b.br.Name, strings.Join(ce.Files, ", "))
			c.logf("combined check: probe %d [%s]: red\n%s", c.res.Probes, strings.Join(ids, " "), failed)
			return probeResult{red: true, failed: failed}, nil
		}
		if err != nil {
			return probeResult{}, fmt.Errorf("combined check: merge %s (%s) into the throwaway worktree: %w", b.task.ID, b.br.Name, err)
		}
		cmds = append(cmds, b.task.Checks...)
	}
	g, err := c.gateInSlot(ctx, gate.Options{
		Host: c.host, Dir: c.dir, Cmds: cmds, LogDir: filepath.Join(c.logDir, fmt.Sprintf("probe-%d", c.res.Probes)),
		Env: c.env, Timeout: c.Cfg.GateTimeout.Duration, Nice: c.Cfg.GateNice, Label: "combined check",
	})
	if err != nil {
		return probeResult{}, err
	}
	if ctx.Err() != nil {
		return probeResult{}, fmt.Errorf("combined check interrupted: %w", ctx.Err())
	}
	if err := cannotRun(g); err != nil {
		return probeResult{}, fmt.Errorf("combined check: the gate could not run on [%s]: %w", strings.Join(ids, " "), err)
	}
	if g.Passed {
		c.logf("combined check: probe %d [%s]: green", c.res.Probes, strings.Join(ids, " "))
		return probeResult{}, nil
	}
	failed := publish.FailedCommands(g)
	c.logf("combined check: probe %d [%s]: red\n%s", c.res.Probes, strings.Join(ids, " "), failed)
	return probeResult{red: true, failed: failed}, nil
}

// combinedGateHolder is the gate slot holder name of a combined check probe.
const combinedGateHolder = "combined check"

// gateInSlot runs one probe's gate inside a gate slot (max_parallel_gates), waiting for a free
// slot first; the slot is released on every path.
func (c *combined) gateInSlot(ctx context.Context, o gate.Options) (g gate.Result, err error) {
	h := store.GateHolder{TaskID: combinedGateHolder, Attempt: c.res.Probes, PID: os.Getpid()}
	onWait := func() { c.logf("combined check: probe %d waits for a free gate slot", c.res.Probes) }
	var release func() error
	if c.Pipeline != nil {
		release, err = c.Pipeline.AcquireGateSlot(ctx, h, onWait)
	} else {
		max := c.Cfg.MaxParallelGates
		if max < 1 {
			max = 1
		}
		release, err = c.Store.AcquireGateSlot(ctx, h, max, pipeline.DefaultGateSlotPoll, onWait)
	}
	if err != nil {
		return g, fmt.Errorf("combined check: %w", err)
	}
	defer func() {
		if rerr := release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("combined check: %w", rerr))
		}
	}()
	return gate.Run(ctx, o), nil
}

// cannotRun reports why a gate result is not a verdict on the code: no commands, or a command
// that could not be started or was interrupted.
func cannotRun(g gate.Result) error {
	if len(g.Commands) == 0 {
		return errors.New(g.Reason)
	}
	for _, cr := range g.Commands {
		if strings.HasPrefix(cr.Error, "could not run") || cr.Error == "interrupted" {
			return fmt.Errorf("%s: %s", cr.Cmd, cr.Error)
		}
	}
	return nil
}

// recordPair appends a combined-conflict event naming both tasks and sets both reasons. The
// statuses are left alone.
func (c *combined) recordPair(ctx context.Context, culprit, partner store.Task, failed string) error {
	goalID := ""
	if culprit.GoalID == partner.GoalID {
		goalID = culprit.GoalID
	}
	if _, err := c.Store.AppendEvent(ctx, "combined-conflict", goalID, culprit.ID, map[string]any{
		"tasks": []string{partner.ID, culprit.ID}, "culprit": culprit.ID, "partner": partner.ID,
		"base": c.base, "failed": failed}); err != nil {
		return fmt.Errorf("record the combined conflict of %s and %s: %w", culprit.ID, partner.ID, err)
	}
	for _, p := range [][2]string{{culprit.ID, partner.ID}, {partner.ID, culprit.ID}} {
		reason := fmt.Sprintf("combined check: merged together with %s onto %s (%s) the gate fails:\n%s",
			p[1], c.Cfg.Remote+"/"+c.Cfg.BaseBranch, short(c.base), failed)
		if err := c.Store.SetTaskReason(ctx, p[0], reason); err != nil {
			return fmt.Errorf("record the combined conflict in the reason of %s: %w", p[0], err)
		}
	}
	c.logf("combined check: %s and %s fail together:\n%s", partner.ID, culprit.ID, failed)
	return nil
}

// removeThrowaway removes the throwaway worktree and git's record of it.
func removeThrowaway(ctx context.Context, repo, dir string) error {
	var errs []error
	if listed, err := worktreeListed(ctx, repo, dir); err != nil {
		errs = append(errs, err)
	} else if listed {
		if err := gitx.RemoveWorktree(ctx, repo, dir); err != nil {
			errs = append(errs, fmt.Errorf("remove the throwaway worktree %s: %w", dir, err))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, fmt.Errorf("delete the throwaway worktree directory %s: %w", dir, err))
	}
	if err := gitx.PruneWorktrees(ctx, repo); err != nil {
		errs = append(errs, fmt.Errorf("prune worktrees after removing %s: %w", dir, err))
	}
	return errors.Join(errs...)
}

// worktreeListed reports whether git records a worktree at dir.
func worktreeListed(ctx context.Context, repo, dir string) (bool, error) {
	out, err := gitx.Run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("list worktrees: %w", err)
	}
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok && filepath.Clean(p) == filepath.Clean(dir) {
			return true, nil
		}
	}
	return false, nil
}

// readyBranches lists the verified and published tasks whose gate ran at their head, ordered
// by goal and then by plan order.
func readyBranches(ctx context.Context, o Options) ([]readyBranch, error) {
	var goals []store.Goal
	if o.Goal != "" {
		g, err := o.Store.GetGoal(ctx, o.Goal)
		if err != nil {
			return nil, fmt.Errorf("goal %s: %w", o.Goal, err)
		}
		goals = []store.Goal{g}
	} else {
		var err error
		if goals, err = o.Store.ListGoals(ctx); err != nil {
			return nil, err
		}
	}
	var out []readyBranch
	for _, g := range goals {
		tasks, err := o.Store.GoalTasks(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		if len(tasks) == 0 {
			continue
		}
		byID := map[string]store.Task{}
		for _, t := range tasks {
			byID[t.ID] = t
		}
		var pl plan.Plan
		if err := json.Unmarshal(g.Plan, &pl); err != nil {
			return nil, fmt.Errorf("goal %s has an unreadable plan: %w", g.ID, err)
		}
		locals, err := pl.Order()
		if err != nil {
			return nil, fmt.Errorf("goal %s plan: %w", g.ID, err)
		}
		for _, l := range locals {
			t, ok := byID[store.TaskID(g.ID, l)]
			if !ok || (t.Status != store.TaskVerified && t.Status != store.TaskPublished) {
				continue
			}
			br, err := o.Store.GetBranch(ctx, t.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", t.ID, t.Status, err)
			}
			if br.Name == "" || br.HeadSHA == "" || br.GateSHA != br.HeadSHA {
				continue
			}
			out = append(out, readyBranch{task: t, br: br})
		}
	}
	return out, nil
}
