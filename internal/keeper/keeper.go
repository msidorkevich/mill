// Package keeper keeps verified and published task branches mergeable (KP4-KP7). One Pass
// syncs with the remote, rebases only the branches that would conflict with their base
// (reusing recorded resolutions with rerere and asking the resolver agent for the rest),
// fixes branches whose gate turned red after a restack, re-publishes published tasks that
// changed and publishes approved tasks whose dependencies now allow it.
//
// It takes no run lock: it claims each task (store.ClaimTasks) before changing it or running
// the pipeline on it, and leaves busy tasks alone, reporting them in Result.Busy.
package keeper

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

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/syncer"
)

// How a rebase's conflicts were resolved (the resolved_by of a task-restacked event).
const (
	ResolvedByRerere = "rerere" // every conflict had a recorded resolution
	ResolvedByAgent  = "agent"  // the resolver agent resolved at least one stop
	ResolvedByNone   = "none"   // the commits replayed without conflicts
)

// Options configure one pass.
type Options struct {
	Store    *store.Store
	Cfg      config.Config
	Paths    project.Paths
	Host     host.Host // runs the gate; nil means host.NewExec()
	Pipeline *pipeline.Pipeline
	Goal     string    // only this goal's tasks; empty means every goal
	Tasks    []string  // only these tasks (full ids); empty means every ready task
	Out      io.Writer // one progress line per action; nil discards
	Stderr   io.Writer // status hook failures; nil discards
	// Now is the clock the CI status query runs on; nil means time.Now.
	Now   func() time.Time
	Force bool // run the combined check even when nothing changed since the last one
	// Claimer claims each task before the pass changes it; the zero value means
	// store.NewClaimer("keep").
	Claimer store.Claimer
	// MaxParallelGates bounds the gates re-run after restacks at the same time (and across
	// every mill process); zero means Cfg.MaxParallelGates.
	MaxParallelGates int
	// GateSlotPoll is how often a gate waiting for a slot checks again; zero means
	// publish.DefaultGateSlotPoll.
	GateSlotPoll time.Duration
}

// Rebased is a task whose own commits were replayed onto its moved base.
type Rebased struct {
	TaskID     string `json:"task_id"`
	Onto       string `json:"onto"`
	OntoSHA    string `json:"onto_sha"`
	OldHead    string `json:"old_head"`
	NewHead    string `json:"new_head"`
	ResolvedBy string `json:"resolved_by"`
}

// Blocked is a task the pass could not rebase, fix or re-publish.
type Blocked struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Skipped is a task the pass did not look at, and why.
type Skipped struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Result is what one pass did.
type Result struct {
	Sync           syncer.Result        `json:"sync"`
	Rebased        []Rebased            `json:"rebased"`
	ResolvedRerere []string             `json:"resolved_rerere"`
	ResolvedAgent  []string             `json:"resolved_agent"`
	Fixed          []string             `json:"fixed"`
	Blocked        []Blocked            `json:"blocked"`
	Republished    []syncer.Republished `json:"republished"`
	// NeedsRepublish lists changed published tasks whose re-publish waits for their parent.
	NeedsRepublish []syncer.NeedsRepublish `json:"needs_republish"`
	Skipped        []Skipped               `json:"skipped"`
	// Queue is what the pass did with approved tasks waiting to be published.
	Queue Queue `json:"queue"`
	// Busy lists tasks the pass left unchanged because they are being worked on (tasks the
	// sync step left alone are in Sync.Busy).
	Busy []syncer.Busy `json:"busy"`
	// CI lists the CI status changes the status hook reported at the end of the pass.
	CI []cistatus.Change `json:"ci"`
}

type keeper struct {
	Options
	upstream string // <remote>/<base_branch>
	res      *Result
	step     *syncer.Step[*Result] // the task being restacked in a pipelined goal; nil runs inline
}

// add appends the lists a goal's restacks fill of o to r.
func (r *Result) add(o *Result) {
	r.Rebased = append(r.Rebased, o.Rebased...)
	r.ResolvedRerere = append(r.ResolvedRerere, o.ResolvedRerere...)
	r.ResolvedAgent = append(r.ResolvedAgent, o.ResolvedAgent...)
	r.Fixed = append(r.Fixed, o.Fixed...)
	r.Blocked = append(r.Blocked, o.Blocked...)
	r.Republished = append(r.Republished, o.Republished...)
	r.NeedsRepublish = append(r.NeedsRepublish, o.NeedsRepublish...)
	r.Skipped = append(r.Skipped, o.Skipped...)
	r.Busy = append(r.Busy, o.Busy...)
}

// publishOptions are the publish options for task id.
func (k *keeper) publishOptions(id string) publish.Options {
	return publish.Options{Store: k.Store, Cfg: k.Cfg, Paths: k.Paths, Host: k.Host, TaskID: id, Out: k.Out,
		MaxParallelGates: k.MaxParallelGates, GateSlotPoll: k.GateSlotPoll}
}

// gateOptions are the publish options for re-running the gate of the claimed task id: its
// claim reads "<command>: waiting for gate" while the gate waits for a slot and
// "<command>: gate" once it holds one. *actErr is set when the claim could not be updated;
// the pass stops with it.
func (k *keeper) gateOptions(ctx context.Context, id string) (po publish.Options, actErr *error) {
	po = k.publishOptions(id)
	actErr = new(error)
	po.GateActivity = func(step string) error {
		*actErr = k.activity(ctx, id, step)
		return *actErr
	}
	return po, actErr
}

func (k *keeper) logf(format string, a ...any) {
	if k.Out != nil {
		fmt.Fprintf(k.Out, format+"\n", a...)
	}
}

// withClaim claims task id, runs fn and releases the claim, also when fn fails. fn runs only
// when the task still has status once claimed. A busy task is listed in Result.Busy and fn
// does not run.
func (k *keeper) withClaim(ctx context.Context, id, status string, fn func() error) error {
	err := k.Store.ClaimTasks(ctx, k.Claimer, []string{id})
	var busy *store.BusyError
	if errors.As(err, &busy) {
		k.res.Busy = append(k.res.Busy, syncer.Busy{TaskID: id, Doing: busy.Doing})
		k.logf("%s: busy (%s); left unchanged", id, busy.Doing)
		return nil
	}
	if err != nil {
		return err
	}
	ferr := func() error {
		t, err := k.Store.GetTask(ctx, id)
		if err != nil || t.Status != status {
			return err // changed since it was read: leave it to the next pass
		}
		return fn()
	}()
	release := func() error { return k.Store.ReleaseClaims(context.WithoutCancel(ctx), k.Claimer, []string{id}) }
	if k.step != nil && k.step.Pending() {
		if ferr == nil {
			// The gate and publish go on in the background; they keep the claim.
			k.step.Start(ctx, release)
			return nil
		}
		k.step.Drop()
	}
	if rerr := release(); rerr != nil {
		return errors.Join(ferr, rerr)
	}
	return ferr
}

// activity records step as what the keeper is doing with the claimed task id, as
// "<command>: <step>".
func (k *keeper) activity(ctx context.Context, id, step string) error {
	if err := k.Store.SetClaimActivity(ctx, k.Claimer, id, k.Claimer.Command+": "+step); err != nil {
		return fmt.Errorf("%s: record step %q on its claim: %w", id, step, err)
	}
	return nil
}

// Pass runs one reconcile pass. Problems with one task block that task and are reported in
// the result; an error means the pass itself could not continue.
func Pass(ctx context.Context, o Options) (Result, error) {
	res := Result{Rebased: []Rebased{}, ResolvedRerere: []string{}, ResolvedAgent: []string{}, Fixed: []string{},
		Blocked: []Blocked{}, Republished: []syncer.Republished{}, NeedsRepublish: []syncer.NeedsRepublish{}, Skipped: []Skipped{},
		Queue: Queue{Published: []QueuePublished{}, Waiting: []QueueWaiting{}}, Busy: []syncer.Busy{}, CI: []cistatus.Change{}}
	if o.Claimer.Owner == "" {
		o.Claimer = store.NewClaimer("keep")
	}
	if o.Pipeline == nil {
		return res, errors.New("keeper: no pipeline to run the resolver, verifier and fixer with")
	}
	sr, err := syncer.Sync(ctx, syncer.Options{Store: o.Store, Cfg: o.Cfg, Paths: o.Paths, Host: o.Host, Out: o.Out, Stderr: o.Stderr, Now: o.Now, SkipCIStatus: true,
		ReportRedGate: true, LeaveFanIn: true, Claimer: o.Claimer, MaxParallelGates: o.MaxParallelGates, GateSlotPoll: o.GateSlotPoll})
	res.Sync = sr
	if err != nil {
		return res, err
	}
	k := &keeper{Options: o, upstream: o.Cfg.Remote + "/" + o.Cfg.BaseBranch, res: &res}

	// KP7 for the branches sync restacked: their gate is red, so the fixer runs.
	for _, gr := range sr.GateRed {
		t, err := k.Store.GetTask(ctx, gr.TaskID)
		if err != nil {
			return res, err
		}
		if err := k.withClaim(ctx, t.ID, t.Status, func() error {
			_, err := k.fix(ctx, t, gr.Gate, t.Status == store.TaskPublished)
			return err
		}); err != nil {
			return res, err
		}
	}

	var goals []store.Goal
	if o.Goal != "" {
		g, err := o.Store.GetGoal(ctx, o.Goal)
		if err != nil {
			return res, fmt.Errorf("goal %s: %w", o.Goal, err)
		}
		goals = []store.Goal{g}
	} else if goals, err = o.Store.ListGoals(ctx); err != nil {
		return res, err
	}
	for _, g := range goals {
		if err := k.goal(ctx, g); err != nil {
			return res, err
		}
	}
	// Approved tasks publish last, once their parents are synced, restacked and fixed.
	if err := k.queue(ctx); err != nil {
		return res, err
	}
	// Last, so that restacked and re-published tasks are queried at their new head.
	changes, err := cistatus.Poll(ctx, cistatus.Options{Store: o.Store, Cfg: o.Cfg, Root: o.Paths.Root, Goal: o.Goal, Out: o.Out, Stderr: o.Stderr, Now: o.Now})
	res.CI = append(res.CI, changes...)
	if err != nil {
		return res, err
	}
	return res, nil
}

// goal walks a goal's tasks parents first.
func (k *keeper) goal(ctx context.Context, g store.Goal) error {
	tasks, err := k.Store.GoalTasks(ctx, g.ID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, t := range tasks {
		known[t.ID] = true
	}
	var pl plan.Plan
	if err := json.Unmarshal(g.Plan, &pl); err != nil {
		return fmt.Errorf("goal %s has an unreadable plan: %w", g.ID, err)
	}
	locals, err := pl.Order()
	if err != nil {
		return fmt.Errorf("goal %s plan: %w", g.ID, err)
	}
	// Each task is restacked in order; its gate and publish run in the background while the
	// next one is restacked. Results and progress are handed back in stack order. The
	// pipeline's progress lines for a task (resolver, verifier, fixer) go with the task's.
	var rt *stepRouter
	if k.Pipeline.Out != nil {
		rt = &stepRouter{out: k.Pipeline.Out, steps: map[string]io.Writer{}}
		k.Pipeline.Out = rt
		defer func() { k.Pipeline.Out = rt.out }()
	}
	emit := func(p *syncer.Step[*Result]) {
		k.res.add(p.Res)
		if k.Out != nil && p.Out.Len() > 0 {
			k.Out.Write(p.Out.Bytes())
		}
	}
	// A child restacked onto its parent's head while the parent's gate went red and the fixer
	// moved it is stale: another round restacks it onto the fixed head. Each round needs a
	// fix, so the rounds are bounded by the tasks.
	for round := 0; ; round++ {
		st := syncer.NewStack[*Result]()
		if rt != nil {
			rt.reset()
		}
		err := k.goalSteps(ctx, g, locals, known, st, rt, emit)
		if err = errors.Join(err, st.Drain(true, emit)); err != nil {
			return err
		}
		stale := st.Stale()
		if len(stale) == 0 {
			return nil
		}
		if round >= len(locals) {
			for _, id := range stale {
				k.skip(id, "its parent's head kept moving during this pass; the next pass restacks it")
			}
			return nil
		}
	}
}

// stepRouter sends the pipeline's progress lines of a task restacked in a pipelined goal to the
// task's step, so that they come out in stack order; other lines go to out. The pipeline writes
// whole lines that start with the task ID.
type stepRouter struct {
	out   io.Writer
	mu    sync.Mutex
	steps map[string]io.Writer
}

func (r *stepRouter) add(id string, w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps[id] = w
}

// reset forgets the steps of the previous round.
func (r *stepRouter) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = map[string]io.Writer{}
}

func (r *stepRouter) Write(b []byte) (int, error) {
	id, _, _ := strings.Cut(string(b), " ")
	id = strings.TrimSuffix(id, ":")
	r.mu.Lock()
	w := r.steps[id]
	r.mu.Unlock()
	if w == nil {
		w = r.out
	}
	return w.Write(b)
}

// goalSteps restacks the goal's tasks in plan order, one step of st each.
func (k *keeper) goalSteps(ctx context.Context, g store.Goal, locals []string, known map[string]bool, st *syncer.Stack[*Result], rt *stepRouter, emit func(*syncer.Step[*Result])) error {
	for _, l := range locals {
		if err := st.Drain(false, emit); err != nil {
			return err
		}
		id := store.TaskID(g.ID, l)
		if !known[id] {
			continue
		}
		// Reload: an earlier step may have changed it.
		t, err := k.Store.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if t.Status != store.TaskVerified && t.Status != store.TaskPublished {
			continue
		}
		br, err := k.Store.GetBranch(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", id, t.Status, err)
		}
		if br.Name == "" {
			continue
		}
		deps, err := k.Store.TaskDeps(ctx, id)
		if err != nil {
			return err
		}
		p := st.Add(id, deps, &Result{})
		tk := &keeper{Options: k.Options, upstream: k.upstream, res: p.Res, step: p}
		tk.Out = &p.Out
		if rt != nil {
			rt.add(id, &p.Out)
		}
		err = tk.task(ctx, t, br)
		p.Finish()
		if err != nil {
			return err
		}
	}
	return nil
}

// task works out the base tip of one task and rebases it when it would conflict with it.
func (k *keeper) task(ctx context.Context, t store.Task, br store.Branch) error {
	deps, err := k.Store.TaskDeps(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(deps) > 1 {
		return k.restackFanIn(ctx, t, br)
	}
	onto, landed, stacked := k.upstream, false, false
	var tip string
	if len(deps) == 1 {
		dep := deps[0]
		dt, err := k.Store.GetTask(ctx, dep)
		if err != nil {
			return err
		}
		db, err := k.Store.GetBranch(ctx, dep)
		if err != nil {
			return fmt.Errorf("dependency %s of %s has no branch: %w", dep, t.ID, err)
		}
		switch {
		case dt.Status == store.TaskLanded:
			landed = br.Base == db.Name
		case store.Done(dt.Status) && db.HeadSHA != "":
			onto, tip, stacked = db.Name, db.HeadSHA, true
		default:
			k.skip(t.ID, fmt.Sprintf("dependency %s is %s; waiting for it", dep, dt.Status))
			return nil
		}
	}
	if tip == "" {
		if tip, err = gitx.RevParse(ctx, k.Paths.Root, "refs/remotes/"+k.upstream); err != nil {
			return fmt.Errorf("remote %s has no branch %s: %w", k.Cfg.Remote, k.Cfg.BaseBranch, err)
		}
	}
	if br.HeadSHA == "" {
		k.skip(t.ID, "no recorded head commit")
		return nil
	}

	// KP4/KP5: a branch that still merges cleanly is left alone.
	root := k.Paths.Root
	in, err := gitx.IsAncestor(ctx, root, tip, br.HeadSHA)
	if err != nil {
		return fmt.Errorf("compare %s with %s: %w", t.ID, onto, err)
	}
	if in {
		return nil
	}
	files, err := gitx.MergeConflicts(ctx, root, tip, br.HeadSHA)
	if err != nil {
		return fmt.Errorf("conflict check of %s against %s: %w", t.ID, onto, err)
	}
	if len(files) == 0 {
		if !stacked {
			return nil
		}
		// A branch stacked on a parent whose head moved (restacked or fixed earlier in this
		// pass) is restacked even without a conflict: published from a branch that lacks the
		// parent's head, it would be stacked on a stale parent.
		return k.withClaim(ctx, t.ID, t.Status, func() error {
			k.logf("%s: does not hold the head of %s (%s); rebasing", t.ID, onto, short(tip))
			return k.rebase(ctx, t, br, onto, tip, landed)
		})
	}
	return k.withClaim(ctx, t.ID, t.Status, func() error {
		k.logf("%s: conflicts with %s (%s) in %s; rebasing", t.ID, onto, short(tip), strings.Join(files, ", "))
		return k.rebase(ctx, t, br, onto, tip, landed)
	})
}

func (k *keeper) skip(id, reason string) {
	k.res.Skipped = append(k.res.Skipped, Skipped{TaskID: id, Reason: reason})
	k.logf("%s: skipped: %s", id, reason)
}

func (k *keeper) block(ctx context.Context, id, reason string) error {
	if err := k.Store.SetTaskStatus(ctx, id, store.TaskBlocked, "", reason); err != nil {
		return fmt.Errorf("block %s (%s): %w", id, reason, err)
	}
	k.res.Blocked = append(k.res.Blocked, Blocked{TaskID: id, Reason: reason})
	k.logf("%s: blocked: %s", id, reason)
	return nil
}

// blockRestack blocks the verified or published task t because its restack failed, recording
// its status so that a later sync retries the restack and restores it.
func (k *keeper) blockRestack(ctx context.Context, t store.Task, reason string) error {
	if err := k.Store.BlockRestack(ctx, t.ID, t.Status, reason); err != nil {
		return fmt.Errorf("block %s (%s): %w", t.ID, reason, err)
	}
	k.res.Blocked = append(k.res.Blocked, Blocked{TaskID: t.ID, Reason: reason})
	k.logf("%s: blocked: %s", t.ID, reason)
	return nil
}

// preflight checks the task's worktree can be rebased onto onto: it has a starting point and
// is a clean checkout of the branch at the recorded head. A failed check blocks the task and
// returns ok false.
func (k *keeper) preflight(ctx context.Context, t store.Task, br store.Branch, onto string) (bool, error) {
	if err := k.activity(ctx, t.ID, "restack"); err != nil {
		return false, err
	}
	if br.StartSHA == "" && br.BaseSHA == "" {
		return false, k.block(ctx, t.ID, "keeper: no recorded starting point, so its own commits cannot be told apart; rebase it by hand")
	}
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return false, k.block(ctx, t.ID, fmt.Sprintf("keeper: worktree %s is not a checkout of %s, so it cannot be rebased onto %s; restore it", br.Worktree, br.Name, onto))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, k.block(ctx, t.ID, fmt.Sprintf("keeper: worktree %s has uncommitted changes, so it cannot be rebased onto %s; commit or discard them", br.Worktree, onto))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return false, err
	}
	if head != br.HeadSHA {
		return false, k.block(ctx, t.ID, fmt.Sprintf("keeper: branch %s is at %s but mill recorded head %s; it was not rebased", br.Name, short(head), short(br.HeadSHA)))
	}
	return true, nil
}

// baseCommits summarises the commits in each of tips that the branch head lacks, for the
// resolver's prompt.
func (k *keeper) baseCommits(ctx context.Context, from string, tips ...string) ([]prompt.BaseCommit, error) {
	var out []prompt.BaseCommit
	seen := map[string]bool{}
	for _, tip := range tips {
		sums, err := gitx.CommitSummaries(ctx, k.Paths.Root, from, tip)
		if err != nil {
			return nil, err
		}
		for _, c := range sums {
			if !seen[c.SHA] {
				seen[c.SHA] = true
				out = append(out, prompt.BaseCommit{SHA: c.SHA, Subject: c.Subject, Trailers: strings.Join(c.Trailers, "\n")})
			}
		}
	}
	return out, nil
}

// resolve asks the resolver agent to resolve the conflicted files in wt and checks no
// conflict markers are left. The returned step and detail say what failed.
func (k *keeper) resolve(ctx context.Context, t store.Task, wt, onto string, baseCommits []prompt.BaseCommit, files []string) (step string, marked []string, detail string) {
	in := prompt.ResolverInput{Task: t.Spec, BaseCommits: baseCommits, Onto: onto}
	for _, f := range files {
		h, err := gitx.ConflictHunks(ctx, wt, f)
		if err != nil {
			return "reading the conflicts", files, err.Error()
		}
		in.Conflicts = append(in.Conflicts, prompt.ConflictFile{Path: f, Hunks: h})
	}
	out, err := k.Pipeline.Resolve(ctx, t.ID, in, wt)
	if err != nil {
		return "the resolver", files, err.Error()
	}
	if out.Stop != "" {
		return "the resolver", files, "it stopped: " + out.Stop
	}
	if out.Reason != "" {
		return "the resolver", files, out.Reason
	}
	left, err := gitx.HasConflictMarkers(ctx, wt, files)
	if err != nil {
		return "checking the resolution", files, err.Error()
	}
	if len(left) > 0 {
		return "the resolver", left, "conflict markers are left"
	}
	return "", nil, ""
}

// rebase replays the net change of the task's own commits onto tip (KP6), resolving stops
// with rerere or the resolver agent. Any failure puts the branch back where it was and blocks the task.
func (k *keeper) rebase(ctx context.Context, t store.Task, br store.Branch, onto, tip string, landed bool) error {
	if ok, err := k.preflight(ctx, t, br, onto); err != nil || !ok {
		return err
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	oldBase := br.BaseSHA
	if oldBase == "" {
		oldBase = oldStart
	}
	baseCommits, err := k.baseCommits(ctx, oldBase, tip)
	if err != nil {
		return err
	}

	old := br
	wt := br.Worktree
	// undo undoes the rebase and blocks the task; step names what failed. A block during the
	// rebase itself (restack) is retried by later syncs.
	undo := func(restack bool, step string, files []string, detail string) error {
		if err := k.restore(ctx, old); err != nil {
			return fmt.Errorf("%s: rebasing onto %s failed (%s: %s) and the branch could not be restored: %w", t.ID, onto, step, detail, err)
		}
		where := ""
		if len(files) > 0 {
			where = " in " + strings.Join(files, ", ")
		}
		reason := fmt.Sprintf("keeper: rebasing onto %s (%s) failed at %s%s: %s; the branch is unchanged. Resolve the conflict by hand, then run `mill task rebase %s` or `mill run --retry-blocked`",
			onto, short(tip), step, where, detail, t.ID)
		if restack {
			return k.blockRestack(ctx, t, reason)
		}
		return k.block(ctx, t.ID, reason)
	}
	fail := func(step string, files []string, detail string) error { return undo(true, step, files, detail) }

	// Replay the net change of the task's commits, so attempts that undo each other cannot
	// conflict on their own.
	if _, _, err := gitx.SquashOwn(ctx, wt, t.ID, oldStart); err != nil {
		return fail("squashing the own commits", nil, err.Error())
	}
	st, err := gitx.RebaseStart(ctx, wt, tip, oldStart, true)
	if err != nil {
		return fail("the rebase", nil, err.Error())
	}
	agent := false
	rerere := st.RerereResolved
	for !st.Done {
		files := st.Files
		k.logf("%s: stopped at %s on conflicts in %s; running the resolver", t.ID, short(st.Stopped), strings.Join(files, ", "))
		agent = true
		if step, marked, detail := k.resolve(ctx, t, wt, onto, baseCommits, files); step != "" {
			return fail(step, marked, detail)
		}
		if st, err = gitx.RebaseContinue(ctx, wt, files, true); err != nil {
			return fail("continuing the rebase", files, err.Error())
		}
		rerere += st.RerereResolved
	}

	if landed {
		br.Base = k.Cfg.BaseBranch
	}
	br.BaseSHA, br.StartSHA, br.HeadSHA = tip, tip, st.Head
	return k.settle(ctx, t, old, br, restacked{onto: onto, tip: tip, oldStart: oldStart, agent: agent, rerere: rerere},
		func(step string, files []string, detail string) error { return undo(false, step, files, detail) })
}

// restacked describes a finished rebase or fan-in restack for settle.
type restacked struct {
	onto, tip, oldStart string
	agent               bool // the resolver agent resolved at least one stop
	rerere              int  // stops rerere resolved
}

// settle records the restacked branch br (formerly old) and checks it: after the resolver the
// gate and the verifier run and a failure goes through fail; otherwise the gate runs and a red
// gate goes to the fixer. A published task is re-published. In a pipelined goal the checks
// run in the background (gateAndPublish) while the next task is restacked.
func (k *keeper) settle(ctx context.Context, t store.Task, old, br store.Branch, r restacked, fail func(step string, files []string, detail string) error) error {
	by := ResolvedByNone
	switch {
	case r.agent:
		by = ResolvedByAgent
	case r.rerere > 0:
		by = ResolvedByRerere
	}

	if err := k.Store.SaveBranch(ctx, br); err != nil {
		return fail("recording the new head", nil, err.Error())
	}
	if k.step != nil {
		k.step.Defer(func(ctx, gctx context.Context) (bool, error) {
			return k.gateAndPublish(ctx, gctx, t, old, br, r, by, fail)
		})
		return nil
	}
	_, err := k.gateAndPublish(ctx, ctx, t, old, br, r, by, fail)
	return err
}

// failedParent waits for the parents restacked in the same goal and returns the first whose
// gate failed, "" when none did (or the task is not pipelined).
func (k *keeper) failedParent(ctx context.Context) (string, error) {
	if k.step == nil {
		return "", nil
	}
	return k.step.WaitParents(ctx)
}

// parentFailedReason is why a restacked task was neither gated nor published.
func parentFailedReason(onto, parent string) string {
	return fmt.Sprintf("keeper: not gated or re-published after rebasing onto %s because the gate of its parent %s failed; every sync retries it once %s is green again",
		onto, parent, parent)
}

// gateAndPublish checks the restacked branch br (formerly old), with gctx for its gate
// (cancelled when a parent's gate fails), and re-publishes a published task. ok reports
// whether it ended green, so that its children may be published. A task whose parent's gate
// failed is neither gated nor published.
func (k *keeper) gateAndPublish(ctx, gctx context.Context, t store.Task, old, br store.Branch, r restacked, by string, fail func(step string, files []string, detail string) error) (bool, error) {
	newHead := br.HeadSHA
	wasPublished := t.Status == store.TaskPublished
	po, actErr := k.gateOptions(ctx, t.ID)
	if r.agent {
		// The agent changed code: the gate and the verifier both check it.
		if err := k.activity(ctx, t.ID, "waiting for gate"); err != nil {
			return false, err
		}
		g, err := publish.GateAtHead(gctx, po, t, br, "keeper")
		if *actErr != nil {
			return false, *actErr
		}
		if parent, perr := k.failedParent(ctx); perr != nil || parent != "" {
			if perr != nil {
				return false, errors.Join(perr, err)
			}
			return false, fail("the gate", nil, parentFailedReason(r.onto, parent))
		}
		if stale, err := k.staleParent(ctx, t, newHead); err != nil || stale {
			return false, err
		}
		if err != nil {
			return false, fail("the gate", nil, err.Error())
		}
		if !g.Passed {
			return false, fail("the gate", nil, "it failed after the resolution:\n"+publish.FailedCommands(g))
		}
		if err := k.activity(ctx, t.ID, "verify"); err != nil {
			return false, err
		}
		v, stop, err := k.Pipeline.Verify(ctx, t.ID)
		if err != nil {
			return false, fail("the verifier", nil, err.Error())
		}
		if stop != "" {
			return false, fail("the verifier", nil, "it stopped: "+stop)
		}
		if v.Verdict != "pass" {
			return false, fail("the verifier", nil, fmt.Sprintf("verdict %s: %s", v.Verdict, oneLine(v.Summary)))
		}
		br.GateSHA = newHead
		if err := k.Store.SaveBranch(ctx, br); err != nil {
			return false, fail("recording the new head", nil, err.Error())
		}
	}

	goalID, _ := store.SplitTaskID(t.ID)
	data := map[string]string{"onto": r.onto, "onto_sha": r.tip, "old_start": r.oldStart, "old_head": old.HeadSHA, "new_head": newHead, "resolved_by": by}
	if br.StartSHA != r.tip {
		data["new_start"] = br.StartSHA
	}
	if _, err := k.Store.AppendEvent(ctx, "task-restacked", goalID, t.ID, data); err != nil {
		return false, err
	}
	k.res.Rebased = append(k.res.Rebased, Rebased{TaskID: t.ID, Onto: r.onto, OntoSHA: r.tip, OldHead: old.HeadSHA, NewHead: newHead, ResolvedBy: by})
	switch by {
	case ResolvedByAgent:
		k.res.ResolvedAgent = append(k.res.ResolvedAgent, t.ID)
	case ResolvedByRerere:
		k.res.ResolvedRerere = append(k.res.ResolvedRerere, t.ID)
	}
	k.logf("%s: rebased onto %s (%s), head %s -> %s (resolved by %s)", t.ID, r.onto, short(r.tip), short(old.HeadSHA), short(newHead), by)

	if !r.agent {
		// KP3: after a rebase without the agent the gate is re-run, the verifier is not.
		if err := k.activity(ctx, t.ID, "waiting for gate"); err != nil {
			return false, err
		}
		g, err := publish.GateAtHead(gctx, po, t, br, "keeper")
		if *actErr != nil {
			return false, *actErr
		}
		if parent, perr := k.failedParent(ctx); perr != nil || parent != "" {
			if perr != nil {
				return false, errors.Join(perr, err)
			}
			return false, k.blockRestack(ctx, t, parentFailedReason(r.onto, parent))
		}
		if stale, serr := k.staleParent(ctx, t, newHead); serr != nil || stale {
			return false, serr
		}
		if err != nil {
			return false, k.block(ctx, t.ID, fmt.Sprintf("keeper: the gate could not be re-run after the rebase: %v", err))
		}
		if !g.Passed {
			// A fixed task is green again (at a new head, which its stale children are
			// restacked onto in another round).
			return k.fix(ctx, t, g, wasPublished)
		}
		br.GateSHA = newHead
		if err := k.Store.SaveBranch(ctx, br); err != nil {
			return false, fmt.Errorf("record the gate result of %s at %s: %w", t.ID, short(newHead), err)
		}
	}
	if !wasPublished {
		return true, nil
	}
	return true, k.republish(ctx, t.ID)
}

// restore puts the branch back at the old head: it aborts a rebase in progress, resets a
// finished one, restores the branch row and checks the worktree is clean at the old head.
func (k *keeper) restore(ctx context.Context, old store.Branch) error {
	wt := old.Worktree
	in, err := rebaseInProgress(ctx, wt)
	if err != nil {
		return err
	}
	if in {
		if err := gitx.RebaseAbort(ctx, wt); err != nil {
			return err
		}
	}
	head, err := gitx.RevParse(ctx, wt, "HEAD")
	if err != nil {
		return fmt.Errorf("read the head of %s: %w", old.Name, err)
	}
	if head != old.HeadSHA {
		if _, err := gitx.Run(ctx, wt, "reset", "-q", "--hard", old.HeadSHA); err != nil {
			return fmt.Errorf("reset %s to %s: %w", old.Name, short(old.HeadSHA), err)
		}
	}
	if head, err = gitx.RevParse(ctx, wt, "HEAD"); err != nil {
		return fmt.Errorf("read the head of %s: %w", old.Name, err)
	}
	if head != old.HeadSHA {
		return fmt.Errorf("branch %s is at %s, not at its old head %s; reset it by hand", old.Name, short(head), short(old.HeadSHA))
	}
	if !gitx.IsWorktreeOn(ctx, wt, old.Name) {
		return fmt.Errorf("worktree %s is not on %s after the restore; check it out by hand", wt, old.Name)
	}
	dirty, err := gitx.HasChanges(ctx, wt)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree %s has changes after the restore; inspect it with `git status`", wt)
	}
	if err := k.Store.SaveBranch(ctx, old); err != nil {
		return fmt.Errorf("record %s back at %s: %w", old.Name, short(old.HeadSHA), err)
	}
	return nil
}

// rebaseInProgress reports whether a rebase is in progress in dir.
func rebaseInProgress(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p, err := gitx.Run(ctx, dir, "rev-parse", "--git-path", name)
		if err != nil {
			return false, fmt.Errorf("locating %s in %s: %w", name, dir, err)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("checking %s: %w", p, err)
		}
	}
	return false, nil
}

// fix runs the fixer for a task whose gate turned red after a restack (KP7): the gate
// failure becomes the findings of an implement attempt.
// fixed reports whether the fixer got the task verified again.
func (k *keeper) fix(ctx context.Context, t store.Task, g gate.Result, wasPublished bool) (fixed bool, err error) {
	if err := k.Pipeline.RecordGateFailure(ctx, t.ID, g); err != nil {
		return false, k.block(ctx, t.ID, fmt.Sprintf("keeper: the gate failed after the restack and its failure could not be recorded for the fixer: %v", err))
	}
	if err := k.activity(ctx, t.ID, "fix"); err != nil {
		return false, err
	}
	k.logf("%s: the gate failed after the restack; running the fixer", t.ID)
	out, err := k.Pipeline.Run(ctx, t.ID)
	if err != nil {
		return false, fmt.Errorf("fixer for %s: %w", t.ID, err)
	}
	if out.Status != store.TaskVerified {
		// The pipeline already recorded the task's status and reason.
		k.res.Blocked = append(k.res.Blocked, Blocked{TaskID: t.ID, Reason: "keeper: the fixer did not get the gate green: " + out.Reason})
		k.logf("%s: blocked: the fixer did not get the gate green: %s", t.ID, out.Reason)
		return false, nil
	}
	k.res.Fixed = append(k.res.Fixed, t.ID)
	k.logf("%s: fixed at %s", t.ID, short(out.HeadSHA))
	if !wasPublished {
		return true, nil
	}
	return true, k.republish(ctx, t.ID)
}

// staleParent reports whether the restacked task t (now at head) sits on a parent head that
// moved since its restack (a parent fixed after its gate went red, or itself stale). Such a
// task is neither gated nor published: its step is marked stale and the goal restacks it again.
func (k *keeper) staleParent(ctx context.Context, t store.Task, head string) (bool, error) {
	if k.step == nil {
		return false, nil
	}
	parent := k.step.StaleParent()
	for _, id := range k.step.ParentIDs() {
		if parent != "" {
			break
		}
		pb, err := k.Store.GetBranch(ctx, id)
		if err != nil {
			return false, fmt.Errorf("parent %s of %s has no branch: %w", id, t.ID, err)
		}
		if pb.HeadSHA == "" {
			continue
		}
		in, err := gitx.IsAncestor(ctx, k.Paths.Root, pb.HeadSHA, head)
		if err != nil {
			return false, fmt.Errorf("compare %s with its parent %s: %w", t.ID, id, err)
		}
		if !in {
			parent = id
		}
	}
	if parent == "" {
		return false, nil
	}
	k.step.MarkStale()
	k.logf("%s: its parent %s moved after this restack; not gated or published, restacking it again", t.ID, parent)
	return true, nil
}

// republish pushes a changed published task again. A refusal by the dependency rule is
// reported as waiting for its parent; any other failure blocks it.
func (k *keeper) republish(ctx context.Context, id string) error {
	if err := k.activity(ctx, id, "publish"); err != nil {
		return err
	}
	po := k.publishOptions(id)
	po.Out = nil // the pass logs the publish itself
	var pr publish.Result
	var err error
	publishIt := func() { pr, err = publish.Publish(ctx, po) }
	if k.step != nil {
		k.step.Serialize(publishIt)
	} else {
		publishIt()
	}
	if parent, ok := publish.ParentRefusal(err); ok {
		k.res.NeedsRepublish = append(k.res.NeedsRepublish, syncer.NeedsRepublish{TaskID: id, After: parent, Reason: err.Error()})
		k.logf("%s: not re-published yet: %v", id, err)
		return nil
	}
	if err != nil {
		return k.block(ctx, id, fmt.Sprintf("keeper: re-publishing failed: %v", err))
	}
	k.res.Republished = append(k.res.Republished, syncer.Republished{TaskID: id, Ref: pr.Ref, SHA: pr.SHA, Parent: pr.Parent})
	k.logf("%s: republished %s at %s", id, pr.Ref, short(pr.SHA))
	publish.PrintHookReport(k.Out, os.Stderr, pr)
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
