// Package fanin restacks a task with several dependencies (a fan-in task) onto them: it resets
// the branch to its deepest parent's head (or to the remote base branch tip when that parent
// landed), merges the other parents that have not landed and replays the net change of the
// task's own commits as one squashed commit.
// Start applies the same rules to the starting point of a new task branch.
//
// It is shared by the keeper, sync and `mill task rebase`; it lives in its own package because
// the keeper imports the syncer. It only reads the store: callers record the new branch row.
package fanin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
)

// Parent is one dependency of a fan-in task.
type Parent struct {
	ID      string
	Branch  string
	Head    string // the recorded head of the dependency's branch
	Landed  bool
	Deepest bool   // the plan parent the task is stacked on
	Landing string // the base-branch commit it landed in (its task-landed event), when known
}

// Target is where a fan-in task is restacked to.
type Target struct {
	Parents []Parent // the deepest parent first
	Onto    string   // the deepest parent's branch, or <remote>/<base_branch> when it landed
	Base    string   // the branch recorded as the task's base: the deepest parent's, or the base branch
	BaseSHA string   // the commit the branch is reset to
	Landed  bool     // the deepest parent landed
	// Upstream is set when a parent other than the deepest landed in a commit the base lacks:
	// the remote base branch tip is merged instead of the landed parent's branch.
	Upstream *Parent
	// Wait is set when a dependency is not verified, published or landed: the task waits.
	Wait string
	// Warning is a non-fatal problem the caller should report, e.g. a failed fetch when the
	// existing remote-tracking base branch was used instead.
	Warning string
}

// Plan works out the target of fan-in task t: its deepest parent per the goal plan and the
// other dependencies to merge. A landed deepest parent is replaced by the remote-tracking
// <remote>/<base_branch>.
func Plan(ctx context.Context, s *store.Store, cfg config.Config, root string, t store.Task) (Target, error) {
	deps, err := s.TaskDeps(ctx, t.ID)
	if err != nil {
		return Target{}, err
	}
	if len(deps) < 2 {
		return Target{}, fmt.Errorf("task %s has %d dependencies; it is not a fan-in task", t.ID, len(deps))
	}
	goalID, local := store.SplitTaskID(t.ID)
	g, err := s.GetGoal(ctx, goalID)
	if err != nil {
		return Target{}, fmt.Errorf("goal of fan-in task %s: %w", t.ID, err)
	}
	var pl plan.Plan
	if err := json.Unmarshal(g.Plan, &pl); err != nil {
		return Target{}, fmt.Errorf("goal %s has an unreadable plan: %w", goalID, err)
	}
	deepest := pl.DeepestParent(local)
	if deepest == "" {
		return Target{}, fmt.Errorf("goal %s plan has no task %s", goalID, local)
	}
	deepest = store.TaskID(goalID, deepest)

	var tg Target
	found := false
	for _, d := range deps {
		dt, err := s.GetTask(ctx, d)
		if err != nil {
			return Target{}, err
		}
		if !store.Done(dt.Status) {
			tg.Wait = fmt.Sprintf("dependency %s is %s; waiting for it", d, dt.Status)
			return tg, nil
		}
		db, err := s.GetBranch(ctx, d)
		if err != nil {
			return Target{}, fmt.Errorf("dependency %s of %s has no branch: %w", d, t.ID, err)
		}
		p := Parent{ID: d, Branch: db.Name, Head: db.HeadSHA, Landed: dt.Status == store.TaskLanded, Deepest: d == deepest}
		if p.Landed {
			if p.Landing, err = landing(ctx, s, root, d); err != nil {
				return Target{}, err
			}
		}
		if !p.Landed && p.Head == "" {
			tg.Wait = fmt.Sprintf("dependency %s has no recorded head commit; waiting for it", d)
			return tg, nil
		}
		if p.Deepest {
			found = true
			tg.Parents = append([]Parent{p}, tg.Parents...)
		} else {
			tg.Parents = append(tg.Parents, p)
		}
	}
	if !found {
		return Target{}, fmt.Errorf("plan parent %s of %s is not among its stored dependencies", deepest, t.ID)
	}
	upstream := cfg.Remote + "/" + cfg.BaseBranch
	upstreamTip := func() (string, error) {
		tip, err := gitx.RevParse(ctx, root, "refs/remotes/"+upstream)
		if err != nil {
			return "", fmt.Errorf("remote %s has no branch %s; fetch it with `git fetch %s`: %w", cfg.Remote, cfg.BaseBranch, cfg.Remote, err)
		}
		return tip, nil
	}
	if p := tg.Parents[0]; p.Landed {
		tip, err := upstreamTip()
		if err != nil {
			return Target{}, err
		}
		tg.Onto, tg.Base, tg.BaseSHA, tg.Landed = upstream, cfg.BaseBranch, tip, true
		return tg, nil
	}
	p := tg.Parents[0]
	tg.Onto, tg.Base, tg.BaseSHA = p.Branch, p.Branch, p.Head
	// A landed parent's changes reach the task through the base branch: merge its tip when
	// the deepest parent does not contain the landing yet.
	for _, q := range tg.Parents[1:] {
		if !q.Landed {
			continue
		}
		in, err := gitx.IsAncestor(ctx, root, q.landed(), tg.BaseSHA)
		if err != nil {
			return Target{}, err
		}
		if !in {
			tip, err := upstreamTip()
			if err != nil {
				return Target{}, err
			}
			tg.Upstream = &Parent{ID: cfg.BaseBranch, Branch: upstream, Head: tip}
			break
		}
	}
	return tg, nil
}

// Start works out what a new branch for task t (with any number of dependencies) starts from.
// A landed dependency's code is already in the base branch, so its own branch is never used:
//   - no dependency, or all landed: the remote base branch tip;
//   - one unlanded: its branch head, merging the remote base branch tip when others landed;
//   - several unlanded: the deepest unlanded one per the goal plan, merging the other unlanded
//     ones and, when any dependency landed, the remote base branch tip.
//
// Start fetches the remote first; without a configured remote the local base branch stands in
// for the remote base branch. Parents lists the unlanded dependencies, the start first; their
// Head is the current head of their branch.
func Start(ctx context.Context, s *store.Store, cfg config.Config, root string, t store.Task) (Target, error) {
	deps, err := s.TaskDeps(ctx, t.ID)
	if err != nil {
		return Target{}, err
	}
	var tg Target
	var unlanded []Parent
	anyLanded := false
	for _, d := range deps {
		dt, err := s.GetTask(ctx, d)
		if err != nil {
			return Target{}, err
		}
		if !store.Done(dt.Status) {
			tg.Wait = fmt.Sprintf("dependency %s is %s, not verified, published or landed; run it first", d, dt.Status)
			return tg, nil
		}
		if dt.Status == store.TaskLanded {
			anyLanded = true
			continue
		}
		db, err := s.GetBranch(ctx, d)
		if err != nil {
			return Target{}, fmt.Errorf("dependency %s is %s but has no branch: %w", d, dt.Status, err)
		}
		head, err := gitx.RevParse(ctx, root, db.Name)
		if err != nil {
			return Target{}, fmt.Errorf("head of dependency branch %q: %w", db.Name, err)
		}
		unlanded = append(unlanded, Parent{ID: d, Branch: db.Name, Head: head})
	}

	var upstream, upstreamTip string
	if anyLanded || len(unlanded) == 0 {
		if upstream, upstreamTip, tg.Warning, err = baseTip(ctx, cfg, root); err != nil {
			return Target{}, err
		}
	}
	if len(unlanded) == 0 {
		tg.Onto, tg.Base, tg.BaseSHA, tg.Landed = upstream, cfg.BaseBranch, upstreamTip, true
		return tg, nil
	}
	start := 0
	if len(unlanded) > 1 {
		goalID, local := store.SplitTaskID(t.ID)
		g, err := s.GetGoal(ctx, goalID)
		if err != nil {
			return Target{}, fmt.Errorf("goal of fan-in task %s: %w", t.ID, err)
		}
		var pl plan.Plan
		if err := json.Unmarshal(g.Plan, &pl); err != nil {
			return Target{}, fmt.Errorf("goal %s has an unreadable plan: %w", goalID, err)
		}
		deepest := deepestUnlanded(pl, local, func(id string) bool {
			for _, p := range unlanded {
				if p.ID == store.TaskID(goalID, id) {
					return true
				}
			}
			return false
		})
		if deepest == "" {
			return Target{}, fmt.Errorf("goal %s plan has no unlanded dependency of task %s", goalID, local)
		}
		start = -1
		for i, p := range unlanded {
			if p.ID == store.TaskID(goalID, deepest) {
				start = i
			}
		}
		if start < 0 {
			return Target{}, fmt.Errorf("plan parent %s of %s is not among its stored dependencies", store.TaskID(goalID, deepest), t.ID)
		}
	}
	unlanded[start].Deepest = true
	tg.Parents = append([]Parent{unlanded[start]}, append(unlanded[:start:start], unlanded[start+1:]...)...)
	p := tg.Parents[0]
	tg.Onto, tg.Base, tg.BaseSHA = p.Branch, p.Branch, p.Head
	if anyLanded {
		tg.Upstream = &Parent{ID: cfg.BaseBranch, Branch: upstream, Head: upstreamTip}
	}
	return tg, nil
}

// deepestUnlanded is plan.DeepestParent restricted to the dependencies of task id for which
// unlanded reports true.
func deepestUnlanded(pl plan.Plan, id string, unlanded func(string) bool) string {
	for _, pt := range pl.Tasks {
		if pt.ID != id {
			continue
		}
		best, bestDepth := "", -1
		for _, d := range pt.DependsOn {
			if !unlanded(d) {
				continue
			}
			if n := pl.Depth(d); n >= bestDepth {
				best, bestDepth = d, n
			}
		}
		return best
	}
	return ""
}

// baseTip fetches the remote and returns <remote>/<base_branch> and its tip. Without a
// configured remote it returns the local base branch and its tip instead. When the fetch fails
// but the remote-tracking branch already exists, its (possibly stale) tip is used and the fetch
// failure is returned as a warning; it fails only when there is no remote-tracking branch.
func baseTip(ctx context.Context, cfg config.Config, root string) (ref, tip, warning string, err error) {
	if cfg.Remote != "" {
		if _, err := gitx.RemoteURL(ctx, root, cfg.Remote); err == nil {
			ref = cfg.Remote + "/" + cfg.BaseBranch
			fetchErr := gitx.Fetch(ctx, root, cfg.Remote)
			tip, err := gitx.RevParse(ctx, root, "refs/remotes/"+ref)
			if fetchErr != nil {
				if err != nil {
					return "", "", "", fmt.Errorf("fetch %s: %w; there is no %s to fall back on, check the remote and retry", cfg.Remote, fetchErr, ref)
				}
				warning = fmt.Sprintf("fetch %s failed (%v); using the existing %s at %.12s, which may be stale", cfg.Remote, oneLine(fetchErr.Error()), ref, tip)
				return ref, tip, warning, nil
			}
			if err != nil {
				return "", "", "", fmt.Errorf("remote %s has no branch %s; push %s to %s first: %w", cfg.Remote, cfg.BaseBranch, cfg.BaseBranch, cfg.Remote, err)
			}
			return ref, tip, "", nil
		}
	}
	tip, err = gitx.RevParse(ctx, root, cfg.BaseBranch)
	if err != nil {
		return "", "", "", fmt.Errorf("base branch %q: %w", cfg.BaseBranch, err)
	}
	return cfg.BaseBranch, tip, "", nil
}

// oneLine joins a multi-line message (such as git's stderr) into one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// landed is the commit that shows the parent has landed: its landing commit, or its head when
// the landing is not recorded.
func (p Parent) landed() string {
	if p.Landing != "" {
		return p.Landing
	}
	return p.Head
}

// landing returns the commit task id landed in per its last task-landed event, or "" when no
// event records one that exists in the repository.
func landing(ctx context.Context, s *store.Store, root, id string) (string, error) {
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return "", err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "task-landed" {
			continue
		}
		var d struct {
			Commit string `json:"commit"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil || d.Commit == "" {
			return "", nil
		}
		if _, err := gitx.RevParse(ctx, root, d.Commit+"^{commit}"); err != nil {
			return "", nil
		}
		return d.Commit, nil
	}
	return "", nil
}

// Moved reports whether the task on br needs a restack onto tg: the deepest parent moved or
// landed since the task was based on it, another parent that has not landed got commits the
// branch lacks, or another parent landed in a commit the branch lacks.
func (tg Target) Moved(ctx context.Context, root string, br store.Branch) (bool, error) {
	if tg.Landed {
		if br.Base != tg.Base {
			return true, nil
		}
	} else if tg.BaseSHA != br.BaseSHA {
		in, err := gitx.IsAncestor(ctx, root, tg.BaseSHA, br.HeadSHA)
		if err != nil {
			return false, err
		}
		if !in {
			return true, nil
		}
	}
	for _, p := range tg.Parents[1:] {
		if !p.Landed {
			in, err := gitx.IsAncestor(ctx, root, p.Head, br.HeadSHA)
			if err != nil {
				return false, err
			}
			if !in {
				return true, nil
			}
			continue
		}
		if p.Landing != "" {
			in, err := gitx.IsAncestor(ctx, root, p.Landing, br.HeadSHA)
			if err != nil {
				return false, err
			}
			if !in {
				return true, nil
			}
			continue
		}
		// The landing is unknown: the branch should not carry the parent's own merge any more
		// unless its base does.
		if p.Head == "" {
			continue
		}
		in, err := gitx.IsAncestor(ctx, root, p.Head, br.HeadSHA)
		if err != nil {
			return false, err
		}
		if !in {
			continue
		}
		inBase, err := gitx.IsAncestor(ctx, root, p.Head, tg.BaseSHA)
		if err != nil {
			return false, err
		}
		if !inBase {
			return true, nil
		}
	}
	return false, nil
}

// Merges are what a restack merges on top of the base: the parents that are not the deepest
// and have not landed, then the remote base branch when a landed parent needs it.
func (tg Target) Merges() []Parent {
	var out []Parent
	for i, p := range tg.Parents {
		if i == 0 {
			continue
		}
		if !p.Landed {
			out = append(out, p)
		}
	}
	if tg.Upstream != nil {
		out = append(out, *tg.Upstream)
	}
	return out
}

// Stop is a step of a restack that stopped on conflicts left in the worktree.
type Stop struct {
	Step  string   // what stopped: "merging <dependency>" or "replaying <commit>"
	Onto  string   // the branch being merged, or what the own commits are replayed onto
	Files []string // the conflicted files
}

// Resolver resolves the conflicted files of a stop in the worktree. An error fails the restack.
type Resolver func(ctx context.Context, stop Stop) error

// Options configure one restack.
type Options struct {
	Branch store.Branch // the task's branch row; its worktree must be clean and at HeadSHA
	Target Target
	// Resolve resolves conflicts; nil means a conflict fails the restack.
	Resolve Resolver
}

// Result is a finished restack.
type Result struct {
	StartSHA string   // the commit after the merges: the task's new starting point
	Head     string   // the new head
	Merged   []string // the dependencies merged again
	Rerere   int      // stops that rerere resolved completely
	Resolved bool     // Resolve resolved at least one stop
}

// ConflictError is a restack that stopped on conflicts that were not resolved. The branch was
// put back at its old head.
type ConflictError struct {
	Step  string
	Onto  string
	Files []string
	Err   error // why the resolver failed; nil when there was no resolver
}

func (e *ConflictError) Error() string {
	msg := fmt.Sprintf("%s conflicts in %s", e.Step, strings.Join(e.Files, ", "))
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *ConflictError) Unwrap() error { return e.Err }

// Restack rebuilds the task's branch on o.Target: it resets to the base, merges the other
// parents that have not landed (with rerere) and replays the net change of the task's own
// commits (the commits after its recorded start that are not merges), squashed into
// one commit. On any failure the branch is put back at its old head; an error from the restore
// is joined to the failure.
func Restack(ctx context.Context, o Options) (Result, error) {
	br, tg := o.Branch, o.Target
	oldStart := br.StartSHA
	if oldStart == "" {
		oldStart = br.BaseSHA
	}
	if oldStart == "" {
		return Result{}, fmt.Errorf("branch %s has no recorded starting point, so its own commits cannot be told apart", br.Name)
	}
	if tg.BaseSHA == "" {
		return Result{}, fmt.Errorf("branch %s has no target to restack onto", br.Name)
	}
	wt := br.Worktree
	var res Result
	fail := func(err error) (Result, error) {
		if rerr := restore(ctx, br); rerr != nil {
			return Result{}, errors.Join(err, fmt.Errorf("the branch could not be restored: %w", rerr))
		}
		return Result{}, err
	}

	// Build the new start on a detached HEAD so the branch keeps the old commits to replay.
	if _, err := gitx.Run(ctx, wt, "checkout", "-q", "--detach", tg.BaseSHA); err != nil {
		return fail(fmt.Errorf("check out %s (%s): %w", tg.Onto, short(tg.BaseSHA), err))
	}
	for _, p := range tg.Merges() {
		msg := fmt.Sprintf("mill: merge %s (dependency %s) into %s", p.Branch, p.ID, br.Name)
		if tg.Upstream != nil && p.Branch == tg.Upstream.Branch {
			msg = fmt.Sprintf("mill: merge %s (landed dependencies) into %s", p.Branch, br.Name)
		}
		ms, err := gitx.MergeStart(ctx, wt, p.Head, msg, true)
		if err != nil {
			return fail(fmt.Errorf("merge dependency %s: %w", p.ID, err))
		}
		if ms.RerereResolved {
			res.Rerere++
		}
		if !ms.Done {
			stop := Stop{Step: "merging " + p.ID, Onto: p.Branch, Files: ms.Files}
			if o.Resolve == nil {
				return fail(&ConflictError{Step: stop.Step, Onto: stop.Onto, Files: stop.Files})
			}
			if err := o.Resolve(ctx, stop); err != nil {
				return fail(&ConflictError{Step: stop.Step, Onto: stop.Onto, Files: stop.Files, Err: err})
			}
			res.Resolved = true
			if _, err := gitx.MergeContinue(ctx, wt, ms.Files, msg); err != nil {
				return fail(fmt.Errorf("commit the merge of dependency %s: %w", p.ID, err))
			}
		}
		res.Merged = append(res.Merged, p.ID)
	}
	start, err := gitx.RevParse(ctx, wt, "HEAD")
	if err != nil {
		return fail(fmt.Errorf("read the new starting point: %w", err))
	}
	res.StartSHA = start

	if _, err := gitx.Run(ctx, wt, "checkout", "-q", br.Name); err != nil {
		return fail(fmt.Errorf("check out %s again: %w", br.Name, err))
	}
	// Replay the net change of the own commits, so attempts that undo each other cannot
	// conflict on their own.
	if _, _, err := gitx.SquashOwn(ctx, wt, br.TaskID, oldStart); err != nil {
		return fail(err)
	}
	st, err := gitx.RebaseStart(ctx, wt, start, oldStart, true)
	if err != nil {
		return fail(fmt.Errorf("replay the own commits: %w", err))
	}
	res.Rerere += st.RerereResolved
	for !st.Done {
		stop := Stop{Step: "replaying " + short(st.Stopped), Onto: tg.Onto, Files: st.Files}
		if o.Resolve == nil {
			return fail(&ConflictError{Step: stop.Step, Onto: stop.Onto, Files: stop.Files})
		}
		if err := o.Resolve(ctx, stop); err != nil {
			return fail(&ConflictError{Step: stop.Step, Onto: stop.Onto, Files: stop.Files, Err: err})
		}
		res.Resolved = true
		if st, err = gitx.RebaseContinue(ctx, wt, st.Files, true); err != nil {
			return fail(fmt.Errorf("continue replaying the own commits: %w", err))
		}
		res.Rerere += st.RerereResolved
	}
	res.Head = st.Head
	return res, nil
}

// restore aborts a merge or rebase in progress and puts the branch back at its old head,
// checked out and clean.
func restore(ctx context.Context, br store.Branch) error {
	wt := br.Worktree
	if ok, err := gitPathExists(ctx, wt, "MERGE_HEAD"); err != nil {
		return err
	} else if ok {
		if err := gitx.MergeAbort(ctx, wt); err != nil {
			return err
		}
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		ok, err := gitPathExists(ctx, wt, name)
		if err != nil {
			return err
		}
		if ok {
			if err := gitx.RebaseAbort(ctx, wt); err != nil {
				return err
			}
			break
		}
	}
	if !gitx.IsWorktreeOn(ctx, wt, br.Name) {
		if _, err := gitx.Run(ctx, wt, "checkout", "-q", "-f", br.Name); err != nil {
			return fmt.Errorf("check out %s in %s: %w", br.Name, wt, err)
		}
	}
	if _, err := gitx.Run(ctx, wt, "reset", "-q", "--hard", br.HeadSHA); err != nil {
		return fmt.Errorf("reset %s to %s: %w", br.Name, short(br.HeadSHA), err)
	}
	head, err := gitx.RevParse(ctx, wt, "HEAD")
	if err != nil {
		return fmt.Errorf("read the head of %s: %w", br.Name, err)
	}
	if head != br.HeadSHA || !gitx.IsWorktreeOn(ctx, wt, br.Name) {
		return fmt.Errorf("worktree %s is not on %s at %s after the restore; check it out by hand", wt, br.Name, short(br.HeadSHA))
	}
	dirty, err := gitx.HasChanges(ctx, wt)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree %s has changes after the restore; inspect it with `git status`", wt)
	}
	return nil
}

// gitPathExists reports whether the git-dir path name (git rev-parse --git-path) exists in dir.
func gitPathExists(ctx context.Context, dir, name string) (bool, error) {
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
	return false, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
