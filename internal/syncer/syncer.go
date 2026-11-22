// Package syncer brings mill's records in line with the remote after reviews merge (GT6, KP2,
// KP3): it marks merged tasks landed (published ones, and verified ones whose branch was merged
// directly), restacks their children, re-gates and re-publishes them, and marks goals landed.
//
// The caller holds the run lock.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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

// GateRed is a restacked task whose gate failed, left unblocked because ReportRedGate is set.
type GateRed struct {
	TaskID string      `json:"task_id"`
	Gate   gate.Result `json:"gate"`
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
	GoalsLanded    []string         `json:"goals_landed"`
	// GateRed lists restacked tasks whose gate failed; only filled when ReportRedGate is set.
	GateRed []GateRed `json:"gate_red,omitempty"`
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

// Sync fetches the remote, marks merged tasks landed, restacks and re-publishes their
// children and marks fully landed goals. Problems with one task block that task and are
// reported in the result; an error means the sync itself could not continue.
func Sync(ctx context.Context, o Options) (Result, error) {
	res := Result{Remote: o.Cfg.Remote, BaseBranch: o.Cfg.BaseBranch,
		Landed: []Landed{}, Restacked: []Restacked{}, Republished: []Republished{}, NeedsRepublish: []NeedsRepublish{}, Blocked: []Blocked{},
		GoalsLanded: []string{}}
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
	return res, nil
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
		if err := s.land(ctx, ts.ID, method, commit, map[string]string{"published_sha": br.PublishedSHA}); err != nil {
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
		if err := s.land(ctx, ts.ID, MethodMerged, commit, map[string]string{"head_sha": br.HeadSHA}); err != nil {
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
		if t.Status != store.TaskVerified && t.Status != store.TaskPublished {
			continue
		}
		if err := s.restack(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// restack rebases one task when its dependency landed or moved (KP2), re-runs the gate (KP3)
// and re-publishes it if it was published.
func (s *syncer) restack(ctx context.Context, t store.Task) error {
	deps, err := s.Store.TaskDeps(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	br, err := s.Store.GetBranch(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", t.ID, t.Status, err)
	}
	if len(deps) > 1 {
		if s.LeaveFanIn {
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
			// Already on the dependency's head; a published task restacked earlier may
			// still wait for its re-publish.
			return s.republishIfStale(ctx, t, br, dt, db)
		}
		onto, ontoSHA = db.Name, db.HeadSHA
	default:
		return nil // the dependency is being reworked; wait for it
	}
	return s.rebase(ctx, t, br, onto, ontoSHA, landed)
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
		return nil
	}
	if ok, err := s.preflight(ctx, t, br, tg.Onto); err != nil || !ok {
		return err
	}
	fr, err := fanin.Restack(ctx, fanin.Options{Branch: br, Target: tg})
	var ce *fanin.ConflictError
	if errors.As(err, &ce) {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: restacking fan-in task onto %s (%s) conflicts at %s in %s; the branch is unchanged. Restack it by hand in %s, then run `mill run --retry-blocked`",
			tg.Onto, short(tg.BaseSHA), ce.Step, strings.Join(ce.Files, ", "), br.Worktree))
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

// preflight checks the task's worktree can be restacked onto onto: it has a starting point,
// is a clean checkout of the branch at the recorded head. A failed check blocks the task.
func (s *syncer) preflight(ctx context.Context, t store.Task, br store.Branch, onto string) (bool, error) {
	if br.StartSHA == "" && br.BaseSHA == "" {
		return false, s.block(ctx, t.ID, "sync: no recorded starting point, so its own commits cannot be told apart; rebase it by hand")
	}
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return false, s.block(ctx, t.ID, fmt.Sprintf("sync: worktree %s is not a checkout of %s, so it cannot be restacked onto %s; restore it", br.Worktree, br.Name, onto))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, s.block(ctx, t.ID, fmt.Sprintf("sync: worktree %s has uncommitted changes, so it cannot be restacked onto %s; commit or discard them", br.Worktree, onto))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return false, err
	}
	if head != br.HeadSHA {
		return false, s.block(ctx, t.ID, fmt.Sprintf("sync: branch %s is at %s but mill recorded head %s; it was not restacked", br.Name, short(head), short(br.HeadSHA)))
	}
	return true, nil
}

// rebase replays the task's own commits onto ontoSHA, re-gates and re-publishes it.
func (s *syncer) rebase(ctx context.Context, t store.Task, br store.Branch, onto, ontoSHA string, landed bool) error {
	if ok, err := s.preflight(ctx, t, br, onto); err != nil || !ok {
		return err
	}
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	newHead, err := gitx.RebaseOnto(ctx, br.Worktree, ontoSHA, oldStart)
	var ce *gitx.RebaseConflictError
	if errors.As(err, &ce) {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: restacking onto %s (%s) conflicts in %s; the branch is unchanged",
			onto, short(ontoSHA), strings.Join(ce.Files, ", ")))
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

	// KP3: the gate is re-run, the verifier is not.
	po := publish.Options{Store: s.Store, Cfg: s.Cfg, Paths: s.Paths, Host: s.Host, TaskID: t.ID}
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
	return s.republish(ctx, t)
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
