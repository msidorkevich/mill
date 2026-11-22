// Package syncer brings mill's records in line with the remote after reviews merge (GT6, KP2,
// KP3): it marks merged tasks landed, restacks their children, re-gates and re-publishes them,
// and marks goals landed.
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

	"mill/internal/config"
	"mill/internal/gitx"
	"mill/internal/host"
	"mill/internal/plan"
	"mill/internal/project"
	"mill/internal/publish"
	"mill/internal/store"
)

// Merge detection methods, in the order they are tried.
const (
	MethodAncestor = "ancestor" // the published commit is in the base branch (merge or fast-forward)
	MethodPatch    = "patch"    // the base branch has a commit with the same patch (rebase or squash)
	MethodTrailer  = "trailer"  // the base branch has a commit with the task's Mill-Task trailer
)

// Options configure one sync.
type Options struct {
	Store *store.Store
	Cfg   config.Config
	Paths project.Paths
	Host  host.Host // runs the gate; nil means host.NewExec()
	Out   io.Writer // one progress line per action; nil discards
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

// Blocked is a task the sync could not restack, gate or re-publish.
type Blocked struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// Manual is a fan-in task that sync does not restack.
type Manual struct {
	TaskID string   `json:"task_id"`
	Deps   []string `json:"deps"`
	Reason string   `json:"reason"`
}

// Result is what one sync did.
type Result struct {
	Remote      string        `json:"remote"`
	BaseBranch  string        `json:"base_branch"`
	BaseSHA     string        `json:"base_sha"` // <remote>/<base_branch> after the fetch
	Landed      []Landed      `json:"landed"`
	Restacked   []Restacked   `json:"restacked"`
	Republished []Republished `json:"republished"`
	Blocked     []Blocked     `json:"blocked"`
	Manual      []Manual      `json:"manual"`
	GoalsLanded []string      `json:"goals_landed"`
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
		Landed: []Landed{}, Restacked: []Restacked{}, Republished: []Republished{}, Blocked: []Blocked{},
		Manual: []Manual{}, GoalsLanded: []string{}}
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

// detect marks every published task that reached the base branch as landed (GT6).
func (s *syncer) detect(ctx context.Context) error {
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
		if err := s.Store.SetTaskStatus(ctx, ts.ID, store.TaskLanded, "", ""); err != nil {
			return fmt.Errorf("%s landed in %s but recording it failed: %w", ts.ID, short(commit), err)
		}
		goalID, _ := store.SplitTaskID(ts.ID)
		if _, err := s.Store.AppendEvent(ctx, "task-landed", goalID, ts.ID,
			map[string]string{"method": method, "commit": commit, "published_sha": br.PublishedSHA, "upstream": s.upstream}); err != nil {
			return fmt.Errorf("%s landed in %s but recording the event failed: %w", ts.ID, short(commit), err)
		}
		s.res.Landed = append(s.res.Landed, Landed{TaskID: ts.ID, Method: method, Commit: commit})
		s.logf("%s: landed in %s (%s)", ts.ID, short(commit), method)
	}
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
		moved, err := s.fanInMoved(ctx, deps, br)
		if err != nil {
			return err
		}
		if moved {
			reason := "fan-in task: its dependencies changed; rebase it by hand or let the keeper do it"
			s.res.Manual = append(s.res.Manual, Manual{TaskID: t.ID, Deps: deps, Reason: reason})
			s.logf("%s: not restacked (%s: %s)", t.ID, strings.Join(deps, ", "), reason)
		}
		return nil
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
		if db.HeadSHA == "" || db.HeadSHA == br.BaseSHA {
			return nil
		}
		onto, ontoSHA = db.Name, db.HeadSHA
	default:
		return nil // the dependency is being reworked; wait for it
	}
	return s.rebase(ctx, t, br, onto, ontoSHA, landed)
}

// fanInMoved reports whether any dependency of a fan-in task landed or moved since the task
// was based on it.
func (s *syncer) fanInMoved(ctx context.Context, deps []string, br store.Branch) (bool, error) {
	for _, d := range deps {
		dt, err := s.Store.GetTask(ctx, d)
		if err != nil {
			return false, err
		}
		if dt.Status == store.TaskLanded {
			return true, nil
		}
		db, err := s.Store.GetBranch(ctx, d)
		if err != nil {
			return false, fmt.Errorf("dependency %s has no branch: %w", d, err)
		}
		if db.HeadSHA == "" {
			continue
		}
		in, err := gitx.IsAncestor(ctx, s.Paths.Root, db.HeadSHA, br.HeadSHA)
		if err != nil {
			return false, err
		}
		if !in {
			return true, nil
		}
	}
	return false, nil
}

func (s *syncer) block(ctx context.Context, id, reason string) error {
	if err := s.Store.SetTaskStatus(ctx, id, store.TaskBlocked, "", reason); err != nil {
		return fmt.Errorf("block %s (%s): %w", id, reason, err)
	}
	s.res.Blocked = append(s.res.Blocked, Blocked{TaskID: id, Reason: reason})
	s.logf("%s: blocked: %s", id, reason)
	return nil
}

// rebase replays the task's own commits onto ontoSHA, re-gates and re-publishes it.
func (s *syncer) rebase(ctx context.Context, t store.Task, br store.Branch, onto, ontoSHA string, landed bool) error {
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	if oldStart == "" {
		return s.block(ctx, t.ID, "sync: no recorded starting point, so its own commits cannot be told apart; rebase it by hand")
	}
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: worktree %s is not a checkout of %s, so it cannot be restacked onto %s; restore it", br.Worktree, br.Name, onto))
	}
	dirty, err := gitx.HasChanges(ctx, br.Worktree)
	if err != nil {
		return err
	}
	if dirty {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: worktree %s has uncommitted changes, so it cannot be restacked onto %s; commit or discard them", br.Worktree, onto))
	}
	head, err := gitx.RevParse(ctx, br.Worktree, "HEAD")
	if err != nil {
		return err
	}
	if head != br.HeadSHA {
		return s.block(ctx, t.ID, fmt.Sprintf("sync: branch %s is at %s but mill recorded head %s; it was not restacked", br.Name, short(head), short(br.HeadSHA)))
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

	oldHead := br.HeadSHA
	if landed {
		br.Base = s.Cfg.BaseBranch
	}
	br.BaseSHA, br.StartSHA, br.HeadSHA = ontoSHA, ontoSHA, newHead
	if err := s.Store.SaveBranch(ctx, br); err != nil {
		return fmt.Errorf("%s was restacked to %s but recording it failed: %w; reset %s to %s by hand or record the new head",
			t.ID, short(newHead), err, br.Name, short(oldHead))
	}
	goalID, _ := store.SplitTaskID(t.ID)
	if _, err := s.Store.AppendEvent(ctx, "task-restacked", goalID, t.ID,
		map[string]string{"onto": onto, "onto_sha": ontoSHA, "old_start": oldStart, "old_head": oldHead, "new_head": newHead}); err != nil {
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
		return s.block(ctx, t.ID, fmt.Sprintf("sync: the gate failed after restacking onto %s:\n%s", onto, publish.FailedCommands(g)))
	}
	br.GateSHA = newHead
	if err := s.Store.SaveBranch(ctx, br); err != nil {
		return fmt.Errorf("record the gate result of %s at %s: %w", t.ID, short(newHead), err)
	}

	if t.Status != store.TaskPublished {
		return nil
	}
	pr, err := publish.Publish(ctx, po)
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
