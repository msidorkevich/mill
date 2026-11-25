// Package syncer brings mill's records in line with the remote after reviews merge (GT6, KP2,
// KP3): it marks merged tasks landed (published ones, and verified ones whose branch was merged
// directly), restacks their children, re-gates and re-publishes them, and marks goals landed.
//
// It takes no run lock: it claims each task (store.ClaimTasks) before changing its records or
// branches and releases the claim after its store updates. A busy task is left unchanged and
// reported in Result.Busy.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/msidorkevich/mill/internal/cleanup"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/fanin"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
)

// Merge detection methods, in the order they are tried.
const (
	MethodAncestor = "ancestor" // the published commit is in the base branch (merge or fast-forward)
	MethodPatch    = "patch"    // the base branch has a commit with the same patch (rebase or squash)
	MethodTrailer  = "trailer"  // the base branch has a commit with the task's Mill-Task trailer
	MethodMerged   = "merged"   // a verified task's branch head is in the base branch (merged without publishing)
)

// Options configure one sync.
type Options struct {
	Store *store.Store
	Cfg   config.Config
	Paths project.Paths
	Host  host.Host // runs the gate; nil means host.NewExec()
	Out   io.Writer // one progress line per action; nil discards
	// ReportRedGate leaves a task whose gate fails after a restack unblocked and lists it in
	// Result.GateRed, for the caller to fix (the keeper). Off, such a task is blocked.
	ReportRedGate bool
	// LeaveFanIn leaves tasks with several dependencies to the caller (the keeper restacks
	// them with conflict handling). Off, a moved fan-in task is restacked and a conflict
	// blocks it.
	LeaveFanIn bool
	// Home is the user's home directory, used to find the worktree root when cleaning up
	// landed tasks; "" means the current user's.
	Home string
	// Claimer claims each task before the sync changes it; the zero value means
	// store.NewClaimer("sync").
	Claimer store.Claimer
}

// Busy is a task the sync would have changed but left alone because it is being worked on.
type Busy struct {
	TaskID string `json:"task_id"`
	Doing  string `json:"doing"` // what holds it, e.g. "attempt 2 (implementer) running"
}

// Landed is a task found merged on the remote.
type Landed struct {
	TaskID string `json:"task_id"`
	Method string `json:"method"`
	Commit string `json:"commit"` // the base-branch commit that contains it
}

// Restacked is a task whose own commits were replayed onto a new base.
type Restacked struct {
	TaskID  string `json:"task_id"`
	Onto    string `json:"onto"`     // the base branch or the dependency's branch
	OntoSHA string `json:"onto_sha"` // the commit it now starts from
	OldHead string `json:"old_head"`
	NewHead string `json:"new_head"`
}

// Republished is a restacked task pushed again to its review branch.
type Republished struct {
	TaskID string `json:"task_id"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	Parent string `json:"parent"`
}

// NeedsRepublish is a published task that was restacked but could not be re-published because
// its parent is not published at its current head. It stays published (its review branch shows
// the old commit) until the parent is published and sync runs again.
type NeedsRepublish struct {
	TaskID string `json:"task_id"`
	After  string `json:"after"` // the parent to publish first
	Reason string `json:"reason"`
}

// Blocked is a task the sync could not restack, gate or re-publish.
type Blocked struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Restored is a task blocked by a restack conflict that got its status back.
type Restored struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // verified or published
}

// GateRed is a restacked task whose gate failed, left unblocked because ReportRedGate is set.
type GateRed struct {
	TaskID string      `json:"task_id"`
	Gate   gate.Result `json:"gate"`
}

// Cleaned is a task landed in this sync whose leftovers were removed (clean_on_land).
type Cleaned struct {
	TaskID       string `json:"task_id"`
	Worktree     string `json:"worktree,omitempty"`      // the worktree removed
	Branch       string `json:"branch,omitempty"`        // the local branch deleted
	RemoteBranch string `json:"remote_branch,omitempty"` // the review branch deleted on the remote
}

// NotCleaned is a cleanup step for a task landed in this sync that was skipped or failed. It
// never fails the sync; `mill clean` retries it.
type NotCleaned struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Result is what one sync did.
type Result struct {
	Remote      string        `json:"remote"`
	BaseBranch  string        `json:"base_branch"`
	BaseSHA     string        `json:"base_sha"` // <remote>/<base_branch> after the fetch
	Landed      []Landed      `json:"landed"`
	Restacked   []Restacked   `json:"restacked"`
	Republished []Republished `json:"republished"`
	// NeedsRepublish lists restacked tasks whose re-publish waits for their parent.
	NeedsRepublish []NeedsRepublish `json:"needs_republish"`
	Blocked        []Blocked        `json:"blocked"`
	// Restored lists tasks blocked by an earlier restack conflict whose restack now
	// succeeded; they are back to the status they had.
	Restored    []Restored `json:"restored"`
	GoalsLanded []string   `json:"goals_landed"`
	// GateRed lists restacked tasks whose gate failed; only filled when ReportRedGate is set.
	GateRed []GateRed `json:"gate_red,omitempty"`
	// Cleaned lists tasks landed in this sync that were cleaned up (clean_on_land);
	// NotCleaned lists the cleanup steps that were skipped or failed.
	Cleaned    []Cleaned    `json:"cleaned"`
	NotCleaned []NotCleaned `json:"not_cleaned"`
	// Busy lists tasks left unchanged because they are being worked on; run sync again later.
	Busy []Busy `json:"busy"`
}

type syncer struct {
	Options
	upstream string // <remote>/<base_branch>
	res      *Result
}

func (s *syncer) logf(format string, a ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, format+"\n", a...)
	}
}

// withClaim claims task id, runs fn and releases the claim, also when fn fails. fn runs only
// when the task still has status once claimed. A busy task is listed in Result.Busy and fn
// does not run.
func (s *syncer) withClaim(ctx context.Context, id, status string, fn func() error) error {
	err := s.Store.ClaimTasks(ctx, s.Claimer, []string{id})
	var busy *store.BusyError
	if errors.As(err, &busy) {
		s.res.Busy = append(s.res.Busy, Busy{TaskID: id, Doing: busy.Doing})
		s.logf("%s: busy (%s); left unchanged", id, busy.Doing)
		return nil
	}
	if err != nil {
		return err
	}
	ferr := func() error {
		t, err := s.Store.GetTask(ctx, id)
		if err != nil || t.Status != status {
			return err // changed since it was read: leave it to the next sync
		}
		return fn()
	}()
	if rerr := s.Store.ReleaseClaims(context.WithoutCancel(ctx), s.Claimer, []string{id}); rerr != nil {
		return errors.Join(ferr, rerr)
	}
	return ferr
}

// newResult is an empty result whose lists are empty rather than nil.
func newResult(o Options) Result {
	return Result{Remote: o.Cfg.Remote, BaseBranch: o.Cfg.BaseBranch,
		Landed: []Landed{}, Restacked: []Restacked{}, Republished: []Republished{}, NeedsRepublish: []NeedsRepublish{}, Blocked: []Blocked{}, Restored: []Restored{},
		GoalsLanded: []string{}, Cleaned: []Cleaned{}, NotCleaned: []NotCleaned{}, Busy: []Busy{}}
}

// Sync fetches the remote, marks merged tasks landed, restacks and re-publishes their
// children and marks fully landed goals. Problems with one task block that task and are
// reported in the result; an error means the sync itself could not continue.
func Sync(ctx context.Context, o Options) (Result, error) {
	res := newResult(o)
	if o.Claimer.Owner == "" {
		o.Claimer = store.NewClaimer("sync")
	}
	root := o.Paths.Root
	if o.Cfg.Remote == "" {
		return res, fmt.Errorf("no remote is set; set `remote` in %s", o.Paths.Config)
	}
	if _, err := gitx.Run(ctx, root, "remote", "get-url", "--", o.Cfg.Remote); err != nil {
		return res, fmt.Errorf("remote %q is not configured in git; add it with `git remote add %s <url>` or set `remote` in %s to an existing remote",
			o.Cfg.Remote, o.Cfg.Remote, o.Paths.Config)
	}
	if err := gitx.Fetch(ctx, root, o.Cfg.Remote); err != nil {
		return res, fmt.Errorf("fetch %s: %w", o.Cfg.Remote, err)
	}
	s := &syncer{Options: o, upstream: o.Cfg.Remote + "/" + o.Cfg.BaseBranch, res: &res}
	tip, err := gitx.RevParse(ctx, root, "refs/remotes/"+s.upstream)
	if err != nil {
		return res, fmt.Errorf("remote %s has no branch %s, so merges cannot be detected; push %s to %s first",
			o.Cfg.Remote, o.Cfg.BaseBranch, o.Cfg.BaseBranch, o.Cfg.Remote)
	}
	res.BaseSHA = tip

	if err := s.detect(ctx); err != nil {
		return res, err
	}
	goals, err := o.Store.ListGoals(ctx)
	if err != nil {
		return res, err
	}
	for _, g := range goals {
		if err := s.restackGoal(ctx, g); err != nil {
			return res, err
		}
	}
	for _, g := range goals {
		if err := s.landGoal(ctx, g); err != nil {
			return res, err
		}
	}
	// Last, so that children of landed tasks were restacked off their branches first.
	s.cleanLanded(ctx)
	return res, nil
}

// Restore gives task id, blocked by a restack conflict and since restacked by hand onto onto
// (`mill task rebase`), back the status it had: it re-runs the gate and re-publishes a
// published task, as a sync does after a restack. The caller holds the task's claim. A task
// not blocked by a restack is left alone.
func Restore(ctx context.Context, o Options, id, onto string) (Result, error) {
	res := newResult(o)
	t, err := o.Store.GetTask(ctx, id)
	if err != nil {
		return res, err
	}
	if !restackBlocked(t) {
		return res, nil
	}
	br, err := o.Store.GetBranch(ctx, id)
	if err != nil {
		return res, fmt.Errorf("task %s is blocked but its branch cannot be loaded: %w", id, err)
	}
	s := &syncer{Options: o, upstream: o.Cfg.Remote + "/" + o.Cfg.BaseBranch, res: &res}
	return res, s.settle(ctx, t, br, onto)
}

// cleanLanded removes what the tasks landed in this sync leave behind, as clean_on_land says.
// A dirty worktree is kept; failures are reported in the result and never stop the sync.
func (s *syncer) cleanLanded(ctx context.Context) {
	mode := s.Cfg.CleanOnLand
	if mode == "off" {
		return
	}
	var candidates []string
	for _, l := range s.res.Landed {
		candidates = append(candidates, l.TaskID)
	}
	holders, extra, err := s.landedDependents(ctx, candidates)
	if err != nil {
		s.res.NotCleaned = append(s.res.NotCleaned, NotCleaned{Reason: fmt.Sprintf("find dependents of landed tasks: %v; run `mill sync` again", err)})
		s.logf("not cleaned up: find dependents of landed tasks: %v", err)
		return
	}
	candidates = append(candidates, extra...)
	if len(candidates) == 0 {
		return
	}
	o := cleanup.Options{Store: s.Store, Root: s.Paths.Root, Remote: s.Cfg.Remote, DeleteRemote: mode == "all", KeepDirty: true, Claimer: s.Claimer}
	notCleaned := func(id, reason string) {
		s.res.NotCleaned = append(s.res.NotCleaned, NotCleaned{TaskID: id, Reason: reason})
		s.logf("%s: not cleaned up: %s", id, reason)
	}
	cleaned := false
	for _, id := range candidates {
		if hs := holders[id]; len(hs) > 0 {
			notCleaned(id, "kept for running dependents: "+strings.Join(hs, ", "))
			continue
		}
		l := Landed{TaskID: id}
		r, err := cleanup.Task(ctx, o, l.TaskID)
		if err != nil {
			notCleaned(l.TaskID, fmt.Sprintf("load its branch: %v; run `mill clean`", err))
			continue
		}
		if r.Busy != "" {
			notCleaned(l.TaskID, fmt.Sprintf("busy (%s); run `mill clean` once it finishes", r.Busy))
			continue
		}
		if r.Kept != "" {
			notCleaned(l.TaskID, r.Kept)
		}
		for _, sk := range r.Skipped {
			if !sk.Expected {
				notCleaned(l.TaskID, sk.Why)
			}
		}
		for _, f := range r.Failures {
			notCleaned(l.TaskID, fmt.Sprintf("%s: %v; fix the cause and run `mill clean`", f.Step, f.Err))
		}
		if !r.Cleaned() {
			continue
		}
		cleaned = true
		s.res.Cleaned = append(s.res.Cleaned, Cleaned{TaskID: l.TaskID, Worktree: r.Worktree, Branch: r.Branch, RemoteBranch: r.RemoteBranch})
		var parts []string
		if r.Worktree != "" {
			parts = append(parts, "worktree "+r.Worktree)
		}
		if r.Branch != "" {
			parts = append(parts, "branch "+r.Branch)
		}
		if r.RemoteBranch != "" {
			parts = append(parts, s.Cfg.Remote+"/"+r.RemoteBranch)
		}
		s.logf("%s: cleaned up %s", l.TaskID, strings.Join(parts, ", "))
	}
	if !cleaned {
		return
	}
	home := s.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			s.res.NotCleaned = append(s.res.NotCleaned, NotCleaned{Reason: fmt.Sprintf("find home directory to prune empty worktree directories: %v", err)})
			s.logf("not cleaned up: find home directory: %v", err)
			return
		}
		home = h
	}
	if _, err := cleanup.PruneEmptyDirs(nil, s.Cfg.Worktrees(s.Paths.Root, home), false); err != nil {
		s.res.NotCleaned = append(s.res.NotCleaned, NotCleaned{Reason: err.Error()})
		s.logf("not cleaned up: %v", err)
	}
}

// landedDependents finds, for the landed tasks in landedNow and for earlier landed tasks whose
// branch still exists, the tasks that keep the branch alive: a dependent that is busy or
// still has the branch as its recorded base. It returns those tasks by landed task, and the
// earlier landed tasks (not in landedNow) that still have a branch to clean up.
func (s *syncer) landedDependents(ctx context.Context, landedNow []string) (map[string][]string, []string, error) {
	now := map[string]bool{}
	for _, id := range landedNow {
		now[id] = true
	}
	busy := map[string]bool{}
	for _, b := range s.res.Busy {
		busy[b.TaskID] = true
	}
	goals, err := s.Store.ListGoals(ctx)
	if err != nil {
		return nil, nil, err
	}
	holders := map[string][]string{}
	var extra []string
	seen := map[string]bool{}
	for _, g := range goals {
		tasks, err := s.Store.GoalTasks(ctx, g.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, t := range tasks {
			if t.Status == store.TaskLanded {
				// An earlier landed task kept for its dependents is cleaned once none holds it.
				if now[t.ID] || seen[t.ID] {
					continue
				}
				lb, err := s.Store.GetBranch(ctx, t.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, nil, fmt.Errorf("landed task %s: load its branch: %w", t.ID, err)
				}
				if gitx.BranchExists(ctx, s.Paths.Root, lb.Name) {
					seen[t.ID] = true
					extra = append(extra, t.ID)
				}
				continue
			}
			deps, err := s.Store.TaskDeps(ctx, t.ID)
			if err != nil {
				return nil, nil, err
			}
			for _, d := range deps {
				dt, err := s.Store.GetTask(ctx, d)
				if err != nil {
					return nil, nil, err
				}
				if dt.Status != store.TaskLanded {
					continue
				}
				db, err := s.Store.GetBranch(ctx, d)
				if err != nil {
					return nil, nil, fmt.Errorf("landed task %s has no branch: %w", d, err)
				}
				tb, err := s.Store.GetBranch(ctx, t.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, nil, fmt.Errorf("task %s: load its branch: %w", t.ID, err)
				}
				if t.Status == store.TaskRunning || busy[t.ID] || (err == nil && tb.Base == db.Name) {
					if !now[d] && !gitx.BranchExists(ctx, s.Paths.Root, db.Name) {
						continue
					}
					holders[d] = append(holders[d], t.ID)
				}
				if !now[d] && !seen[d] && gitx.BranchExists(ctx, s.Paths.Root, db.Name) {
					seen[d] = true
					extra = append(extra, d)
				}
			}
		}
	}
	return holders, extra, nil
}

// detect marks every published task that reached the base branch as landed (GT6), and every
// verified task whose branch was merged into it without being published.
func (s *syncer) detect(ctx context.Context) error {
	if err := s.detectPublished(ctx); err != nil {
		return err
	}
	return s.detectMerged(ctx)
}

func (s *syncer) detectPublished(ctx context.Context) error {
	pub, err := s.Store.TaskSummaries(ctx, store.TaskPublished)
	if err != nil {
		return err
	}
	for _, ts := range pub {
		br, err := s.Store.GetBranch(ctx, ts.ID)
		if err != nil {
			return fmt.Errorf("task %s is published but its branch cannot be loaded: %w", ts.ID, err)
		}
		if br.PublishedSHA == "" {
			return fmt.Errorf("task %s is published but has no recorded published commit; publish it again with `mill review approve %s`", ts.ID, ts.ID)
		}
		method, commit, err := s.landedBy(ctx, ts.ID, br.PublishedSHA)
		if err != nil {
			return fmt.Errorf("merge detection for %s: %w", ts.ID, err)
		}
		if method == "" {
			continue
		}
		err = s.withClaim(ctx, ts.ID, store.TaskPublished, func() error {
			return s.land(ctx, ts.ID, method, commit, map[string]string{"published_sha": br.PublishedSHA})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// detectMerged marks verified tasks landed whose branch head is already in the base branch:
// someone merged the branch itself instead of a published review.
func (s *syncer) detectMerged(ctx context.Context) error {
	ver, err := s.Store.TaskSummaries(ctx, store.TaskVerified)
	if err != nil {
		return err
	}
	root := s.Paths.Root
	for _, ts := range ver {
		br, err := s.Store.GetBranch(ctx, ts.ID)
		if err != nil {
			return fmt.Errorf("task %s is verified but its branch cannot be loaded: %w", ts.ID, err)
		}
		if br.HeadSHA == "" {
			return fmt.Errorf("task %s is verified but has no recorded head commit; run `mill task show %s`", ts.ID, ts.ID)
		}
		// A branch without commits of its own is trivially in the base branch.
		if br.HeadSHA == br.BaseSHA || br.HeadSHA == br.StartSHA {
			continue
		}
		in, err := gitx.IsAncestor(ctx, root, br.HeadSHA, s.upstream)
		if err != nil {
			return fmt.Errorf("merge detection for %s: %w", ts.ID, err)
		}
		if !in {
			continue
		}
		commit, err := gitx.ContainingCommit(ctx, root, s.upstream, br.HeadSHA)
		if err != nil {
			return fmt.Errorf("merge detection for %s: %w", ts.ID, err)
		}
		err = s.withClaim(ctx, ts.ID, store.TaskVerified, func() error {
			return s.land(ctx, ts.ID, MethodMerged, commit, map[string]string{"head_sha": br.HeadSHA})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// land records a task as landed in commit and reports it.
func (s *syncer) land(ctx context.Context, id, method, commit string, data map[string]string) error {
	if err := s.Store.SetTaskStatus(ctx, id, store.TaskLanded, "", ""); err != nil {
		return fmt.Errorf("%s landed in %s but recording it failed: %w", id, short(commit), err)
	}
	data["method"], data["commit"], data["upstream"] = method, commit, s.upstream
	goalID, _ := store.SplitTaskID(id)
	if _, err := s.Store.AppendEvent(ctx, "task-landed", goalID, id, data); err != nil {
		return fmt.Errorf("%s landed in %s but recording the event failed: %w", id, short(commit), err)
	}
	s.res.Landed = append(s.res.Landed, Landed{TaskID: id, Method: method, Commit: commit})
	s.logf("%s: landed in %s (%s)", id, short(commit), method)
	return nil
}

// landedBy tries the three detection methods in order and returns the first that matches and
// the base-branch commit that contains the task. method is "" when the task has not landed.
func (s *syncer) landedBy(ctx context.Context, id, published string) (method, commit string, err error) {
	root := s.Paths.Root
	in, err := gitx.IsAncestor(ctx, root, published, s.upstream)
	if err != nil {
		return "", "", err
	}
	if in {
		c, err := gitx.ContainingCommit(ctx, root, s.upstream, published)
		if err != nil {
			return "", "", err
		}
		return MethodAncestor, c, nil
	}
	same, err := gitx.PatchLanded(ctx, root, s.upstream, published)
	if err != nil {
		return "", "", err
	}
	if same {
		c, ok, err := gitx.FindPatch(ctx, root, s.upstream, published)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("git cherry says %s has the patch of %s, but no commit with that patch was found", s.upstream, short(published))
		}
		return MethodPatch, c, nil
	}
	c, ok, err := gitx.FindTrailer(ctx, root, s.upstream, "Mill-Task: "+id)
	if err != nil {
		return "", "", err
	}
	if ok {
		return MethodTrailer, c, nil
	}
	return "", "", nil
}

// restackGoal walks a goal's tasks parents first and restacks the ones whose base moved.
func (s *syncer) restackGoal(ctx context.Context, g store.Goal) error {
	tasks, err := s.Store.GoalTasks(ctx, g.ID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	byID := map[string]store.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var order []string
	var pl plan.Plan
	if err := json.Unmarshal(g.Plan, &pl); err != nil {
		return fmt.Errorf("goal %s has an unreadable plan: %w", g.ID, err)
	}
	locals, err := pl.Order()
	if err != nil {
		return fmt.Errorf("goal %s plan: %w", g.ID, err)
	}
	for _, l := range locals {
		if _, ok := byID[store.TaskID(g.ID, l)]; ok {
			order = append(order, store.TaskID(g.ID, l))
		}
	}
	for _, id := range order {
		// Reload: an earlier step may have changed it.
		t, err := s.Store.GetTask(ctx, id)
		if err != nil {
			return err
		}
		// A task blocked by a restack conflict is retried: the cause may be gone.
		if t.Status == store.TaskRunning {
			if err := s.noteBusyRestack(ctx, t); err != nil {
				return err
			}
			continue
		}
		if t.Status != store.TaskVerified && t.Status != store.TaskPublished && !restackBlocked(t) {
			continue
		}
		if err := s.restack(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// noteBusyRestack lists running task t in Result.Busy when it sits on the branch of a landed
// dependency, so a restack is due that a running task cannot take. It never changes t.
func (s *syncer) noteBusyRestack(ctx context.Context, t store.Task) error {
	deps, err := s.Store.TaskDeps(ctx, t.ID)
	if err != nil || len(deps) != 1 {
		return err
	}
	dt, err := s.Store.GetTask(ctx, deps[0])
	if err != nil || dt.Status != store.TaskLanded {
		return err
	}
	br, err := s.Store.GetBranch(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", t.ID, t.Status, err)
	}
	db, err := s.Store.GetBranch(ctx, dt.ID)
	if err != nil {
		return fmt.Errorf("dependency %s of %s has no branch: %w", dt.ID, t.ID, err)
	}
	if br.Base != db.Name {
		return nil // already moved off the dependency's branch
	}
	return s.withClaim(ctx, t.ID, t.Status, func() error { return nil })
}

// restackBlocked reports whether t is blocked because a restack of its branch conflicted.
func restackBlocked(t store.Task) bool {
	return t.Status == store.TaskBlocked && t.RestackBlockedFrom != ""
}

// restack rebases one task when its dependency landed or moved (KP2), re-runs the gate (KP3)
// and re-publishes it if it was published. A task blocked by an earlier restack conflict is
// restacked again and, on success, gets back the status it had.
func (s *syncer) restack(ctx context.Context, t store.Task) error {
	deps, err := s.Store.TaskDeps(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(deps) == 0 && !restackBlocked(t) {
		return nil
	}
	br, err := s.Store.GetBranch(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", t.ID, t.Status, err)
	}
	if len(deps) == 0 {
		return s.retryOntoBase(ctx, t, br)
	}
	if len(deps) > 1 {
		// A restack-blocked fan-in task is retried here even when the caller restacks fan-in
		// tasks itself: the caller only looks at verified and published tasks.
		if s.LeaveFanIn && !restackBlocked(t) {
			return nil
		}
		return s.restackFanIn(ctx, t, br)
	}
	dep := deps[0]
	dt, err := s.Store.GetTask(ctx, dep)
	if err != nil {
		return err
	}
	db, err := s.Store.GetBranch(ctx, dep)
	if err != nil {
		return fmt.Errorf("dependency %s of %s has no branch: %w", dep, t.ID, err)
	}
	var onto, ontoSHA string
	landed := false
	switch {
	case dt.Status == store.TaskLanded:
		if br.Base != db.Name {
			if restackBlocked(t) {
				// Blocked restacking onto the base branch (the keeper's restack).
				return s.retryOntoBase(ctx, t, br)
			}
			return nil // already moved off the dependency's branch
		}
		onto, landed = s.Cfg.BaseBranch, true
		if ontoSHA, err = gitx.RevParse(ctx, s.Paths.Root, "refs/remotes/"+s.upstream); err != nil {
			return err
		}
	case store.Done(dt.Status):
		if db.HeadSHA == "" {
			return nil
		}
		if db.HeadSHA == br.BaseSHA {
			if restackBlocked(t) {
				// The dependency is back where the task starts: nothing to restack.
				return s.withClaim(ctx, t.ID, t.Status, func() error { return s.settle(ctx, t, br, db.Name) })
			}
			// Already on the dependency's head; a published task restacked earlier may
			// still wait for its re-publish.
			return s.republishIfStale(ctx, t, br, dt, db)
		}
		onto, ontoSHA = db.Name, db.HeadSHA
	default:
		return nil // the dependency is being reworked; wait for it
	}
	return s.withClaim(ctx, t.ID, t.Status, func() error { return s.rebase(ctx, t, br, onto, ontoSHA, landed) })
}

// retryOntoBase retries the restack of a restack-blocked task that sits on the base branch:
// it is restacked onto the remote base branch unless it already contains it.
func (s *syncer) retryOntoBase(ctx context.Context, t store.Task, br store.Branch) error {
	tip, err := gitx.RevParse(ctx, s.Paths.Root, "refs/remotes/"+s.upstream)
	if err != nil {
		return err
	}
	if br.HeadSHA == "" {
		return nil
	}
	in, err := gitx.IsAncestor(ctx, s.Paths.Root, tip, br.HeadSHA)
	if err != nil {
		return fmt.Errorf("compare %s with %s: %w", t.ID, s.upstream, err)
	}
	if in {
		return s.withClaim(ctx, t.ID, t.Status, func() error { return s.settle(ctx, t, br, s.Cfg.BaseBranch) })
	}
	return s.withClaim(ctx, t.ID, t.Status, func() error { return s.rebase(ctx, t, br, s.Cfg.BaseBranch, tip, false) })
}

// restackFanIn restacks a fan-in task whose dependencies moved or landed onto its deepest
// parent, merging the others again. Conflicts are not resolved: they block the task and leave
// its branch unchanged.
func (s *syncer) restackFanIn(ctx context.Context, t store.Task, br store.Branch) error {
	tg, err := fanin.Plan(ctx, s.Store, s.Cfg, s.Paths.Root, t)
	if err != nil {
		return err
	}
	if tg.Wait != "" || br.HeadSHA == "" {
		return nil
	}
	moved, err := tg.Moved(ctx, s.Paths.Root, br)
	if err != nil {
		return fmt.Errorf("compare fan-in task %s with its dependencies: %w", t.ID, err)
	}
	if !moved {
		if restackBlocked(t) {
			// The dependencies are back where the task starts: nothing to restack.
			return s.withClaim(ctx, t.ID, t.Status, func() error { return s.settle(ctx, t, br, tg.Onto) })
		}
		return nil
	}
	return s.withClaim(ctx, t.ID, t.Status, func() error { return s.restackFanInClaimed(ctx, t, br, tg) })
}

// restackFanInClaimed restacks the claimed fan-in task t onto tg.
func (s *syncer) restackFanInClaimed(ctx context.Context, t store.Task, br store.Branch, tg fanin.Target) error {
	if ok, err := s.preflight(ctx, t, br, tg.Onto); err != nil || !ok {
		return err
	}
	fr, err := fanin.Restack(ctx, fanin.Options{Branch: br, Target: tg})
	var ce *fanin.ConflictError
	if errors.As(err, &ce) {
		return s.blockRestack(ctx, t, fmt.Sprintf("sync: restacking fan-in task onto %s (%s) conflicts at %s in %s; the branch is unchanged. Every sync retries the restack; or restack it by hand in %s, then run `mill task rebase %s`",
			tg.Onto, short(tg.BaseSHA), ce.Step, strings.Join(ce.Files, ", "), br.Worktree, t.ID))
	}
	if err != nil {
		return fmt.Errorf("restack %s onto %s: %w", t.ID, tg.Onto, err)
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	old := br
	br.Base, br.BaseSHA, br.StartSHA, br.HeadSHA = tg.Base, tg.BaseSHA, fr.StartSHA, fr.Head
	return s.finish(ctx, t, old, br, tg.Onto, oldStart)
}

func (s *syncer) block(ctx context.Context, id, reason string) error {
	if err := s.Store.SetTaskStatus(ctx, id, store.TaskBlocked, "", reason); err != nil {
		return fmt.Errorf("block %s (%s): %w", id, reason, err)
	}
	s.res.Blocked = append(s.res.Blocked, Blocked{TaskID: id, Reason: reason})
	s.logf("%s: blocked: %s", id, reason)
	return nil
}

// blockRestack blocks t because its restack could not be done, recording the status it had
// (or had before an earlier restack blocked it) so that a later sync retries the restack.
func (s *syncer) blockRestack(ctx context.Context, t store.Task, reason string) error {
	from := t.Status
	if restackBlocked(t) {
		from = t.RestackBlockedFrom
	}
	if err := s.Store.BlockRestack(ctx, t.ID, from, reason); err != nil {
		return fmt.Errorf("block %s (%s): %w", t.ID, reason, err)
	}
	s.res.Blocked = append(s.res.Blocked, Blocked{TaskID: t.ID, Reason: reason})
	s.logf("%s: blocked: %s", t.ID, reason)
	return nil
}

// blockPreflight blocks t because its worktree cannot be restacked. A task already blocked by
// a restack stays a restack block, so later syncs keep retrying it.
func (s *syncer) blockPreflight(ctx context.Context, t store.Task, reason string) error {
	if restackBlocked(t) {
		return s.blockRestack(ctx, t, reason)
	}
	return s.block(ctx, t.ID, reason)
}

// preflight checks the task's worktree can be restacked onto onto: it has a starting point,
// is a clean checkout of the branch at the recorded head. A failed check blocks the task.
func (s *syncer) preflight(ctx context.Context, t store.Task, br store.Branch, onto string) (bool, error) {
	if br.StartSHA == "" && br.BaseSHA == "" {
		return false, s.blockPreflight(ctx, t, "sync: no recorded starting point, so its own commits cannot be told apart; rebase it by hand")
	}
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return false, s.blockPreflight(ctx, t, fmt.Sprintf("sync: worktree %s is not a checkout of %s, so it cannot be restacked onto %s; restore it", br.Worktree, br.Name, onto))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, s.blockPreflight(ctx, t, fmt.Sprintf("sync: worktree %s has uncommitted changes, so it cannot be restacked onto %s; commit or discard them", br.Worktree, onto))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return false, err
	}
	if head != br.HeadSHA {
		return false, s.blockPreflight(ctx, t, fmt.Sprintf("sync: branch %s is at %s but mill recorded head %s; it was not restacked", br.Name, short(head), short(br.HeadSHA)))
	}
	return true, nil
}

// rebase replays the net change of the task's own commits onto ontoSHA, re-gates and
// re-publishes it.
func (s *syncer) rebase(ctx context.Context, t store.Task, br store.Branch, onto, ontoSHA string, landed bool) error {
	if ok, err := s.preflight(ctx, t, br, onto); err != nil || !ok {
		return err
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	newHead, err := gitx.ReplayOnto(ctx, br.Worktree, t.ID, ontoSHA, oldStart)
	var ce *gitx.RebaseConflictError
	if errors.As(err, &ce) {
		return s.blockRestack(ctx, t, fmt.Sprintf("sync: restacking onto %s (%s) conflicts in %s; the branch is unchanged. Every sync retries the restack; or rebase it by hand in %s, then run `mill task rebase %s`",
			onto, short(ontoSHA), strings.Join(ce.Files, ", "), br.Worktree, t.ID))
	}
	if err != nil {
		return fmt.Errorf("restack %s onto %s: %w", t.ID, onto, err)
	}

	old := br
	if landed {
		br.Base = s.Cfg.BaseBranch
	}
	br.BaseSHA, br.StartSHA, br.HeadSHA = ontoSHA, ontoSHA, newHead
	return s.finish(ctx, t, old, br, onto, oldStart)
}

// finish records a restacked branch (br, formerly old), re-gates and re-publishes it.
func (s *syncer) finish(ctx context.Context, t store.Task, old, br store.Branch, onto, oldStart string) error {
	oldHead, newHead, ontoSHA := old.HeadSHA, br.HeadSHA, br.BaseSHA
	if err := s.Store.SaveBranch(ctx, br); err != nil {
		return fmt.Errorf("%s was restacked to %s but recording it failed: %w; reset %s to %s by hand or record the new head",
			t.ID, short(newHead), err, br.Name, short(oldHead))
	}
	goalID, _ := store.SplitTaskID(t.ID)
	if _, err := s.Store.AppendEvent(ctx, "task-restacked", goalID, t.ID,
		map[string]string{"onto": onto, "onto_sha": ontoSHA, "old_start": oldStart, "new_start": br.StartSHA, "old_head": oldHead, "new_head": newHead}); err != nil {
		return err
	}
	s.res.Restacked = append(s.res.Restacked, Restacked{TaskID: t.ID, Onto: onto, OntoSHA: ontoSHA, OldHead: oldHead, NewHead: newHead})
	s.logf("%s: restacked onto %s (%s), head %s -> %s", t.ID, onto, short(ontoSHA), short(oldHead), short(newHead))
	return s.settle(ctx, t, br, onto)
}

// settle checks a restacked task t at br's head: a task blocked by an earlier restack gets
// back the status it had, the gate is re-run and a published task is re-published.
func (s *syncer) settle(ctx context.Context, t store.Task, br store.Branch, onto string) error {
	newHead := br.HeadSHA
	if restackBlocked(t) {
		from := t.RestackBlockedFrom
		if err := s.Store.SetTaskStatus(ctx, t.ID, from, "", ""); err != nil {
			return fmt.Errorf("%s was restacked but restoring its status %s failed: %w", t.ID, from, err)
		}
		t.Status, t.Reason, t.RestackBlockedFrom = from, "", ""
		s.res.Restored = append(s.res.Restored, Restored{TaskID: t.ID, Status: from})
		s.logf("%s: restack succeeded; %s again", t.ID, from)
	}

	// KP3: the gate is re-run, the verifier is not.
	po := publish.Options{Store: s.Store, Cfg: s.Cfg, Paths: s.Paths, Host: s.Host, TaskID: t.ID, Out: s.Out}
	g, err := publish.GateAtHead(ctx, po, t, br, "sync")
	if err != nil {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: the gate could not be re-run after the restack: %v", err))
	}
	if !g.Passed {
		if s.ReportRedGate {
			s.res.GateRed = append(s.res.GateRed, GateRed{TaskID: t.ID, Gate: g})
			s.logf("%s: the gate failed after restacking onto %s; left for the fixer", t.ID, onto)
			return nil
		}
		return s.block(ctx, t.ID, fmt.Sprintf("sync: the gate failed after restacking onto %s:\n%s", onto, publish.FailedCommands(g)))
	}
	br.GateSHA = newHead
	if err := s.Store.SaveBranch(ctx, br); err != nil {
		return fmt.Errorf("record the gate result of %s at %s: %w", t.ID, short(newHead), err)
	}

	if t.Status != store.TaskPublished {
		return nil
	}
	return s.republish(ctx, t)
}

// republishIfStale re-publishes a published task whose published commit no longer matches its
// branch head or its published parent: a restack whose re-publish waited for the parent.
func (s *syncer) republishIfStale(ctx context.Context, t store.Task, br store.Branch, dt store.Task, db store.Branch) error {
	if t.Status != store.TaskPublished || br.PublishedSHA == "" {
		return nil
	}
	root := s.Paths.Root
	stale := false
	if dt.Status == store.TaskPublished && db.PublishedSHA != "" {
		parent, err := gitx.RevParse(ctx, root, br.PublishedSHA+"^")
		if err != nil {
			return fmt.Errorf("read the parent of %s's published commit %s: %w", t.ID, short(br.PublishedSHA), err)
		}
		stale = parent != db.PublishedSHA
	}
	if !stale {
		pubTree, err := gitx.TreeOf(ctx, root, br.PublishedSHA)
		if err != nil {
			return err
		}
		headTree, err := gitx.TreeOf(ctx, root, br.HeadSHA)
		if err != nil {
			return err
		}
		stale = pubTree != headTree
	}
	if !stale {
		return nil
	}
	return s.withClaim(ctx, t.ID, t.Status, func() error { return s.republish(ctx, t) })
}

// republish pushes a published task again. A refusal by the dependency rule leaves it
// published and reports it as waiting for its parent; any other failure blocks it.
func (s *syncer) republish(ctx context.Context, t store.Task) error {
	po := publish.Options{Store: s.Store, Cfg: s.Cfg, Paths: s.Paths, Host: s.Host, TaskID: t.ID}
	pr, err := publish.Publish(ctx, po)
	if parent, ok := publish.ParentRefusal(err); ok {
		next := fmt.Sprintf("publish %s with `mill review approve %s`, then run `mill sync`", parent, parent)
		if parent == "" {
			next = "publish its dependencies, then run `mill sync`"
		}
		if publish.RestackRefusal(err) {
			next = "run `mill sync` again to restack it onto " + parent
		}
		s.res.NeedsRepublish = append(s.res.NeedsRepublish, NeedsRepublish{TaskID: t.ID, After: parent, Reason: err.Error()})
		s.logf("%s: not re-published yet: %v; %s", t.ID, err, next)
		return nil
	}
	if err != nil {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: re-publishing after the restack failed: %v", err))
	}
	s.res.Republished = append(s.res.Republished, Republished{TaskID: t.ID, Ref: pr.Ref, SHA: pr.SHA, Parent: pr.Parent})
	s.logf("%s: republished %s at %s (parent %s)", t.ID, pr.Ref, short(pr.SHA), short(pr.Parent))
	return nil
}

// landGoal marks a goal landed when every one of its tasks has landed.
func (s *syncer) landGoal(ctx context.Context, g store.Goal) error {
	if g.Status == store.GoalLanded || g.Status == store.GoalDraft {
		return nil
	}
	tasks, err := s.Store.GoalTasks(ctx, g.ID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if t.Status != store.TaskLanded {
			return nil
		}
	}
	if err := s.Store.SetGoalStatus(ctx, g.ID, store.GoalLanded, ""); err != nil {
		return fmt.Errorf("every task of goal %s landed but recording it failed: %w", g.ID, err)
	}
	s.res.GoalsLanded = append(s.res.GoalsLanded, g.ID)
	s.logf("goal %s: landed", g.ID)
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
