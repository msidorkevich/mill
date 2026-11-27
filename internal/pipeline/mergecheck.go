package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// OutcomeReopened is the Outcome status of a run whose task was verified and then sent back
// to pending by the merge check: the run reported a result, so a scheduler runs it again.
const OutcomeReopened = "reopened"

// MergeCheckFailedEvent is the kind of the event recorded when the merge check sends a task
// back to pending.
const MergeCheckFailedEvent = "merge-check-failed"

// mergeSibling is a verified, unlanded task of the same goal whose changes the merge check
// merges with the task just verified.
type mergeSibling struct {
	id     string
	branch string
}

// mergeClash is one reason the merge check failed: a merge conflict (files) or a failed
// merge_check_cmd (cmd).
type mergeClash struct {
	siblings []string
	files    []string        // conflicting files, when a merge conflicted
	cmd      *gate.CmdResult // the failed command, when the merge succeeded
}

// mergeCheck runs merge_check_cmd on the task just verified (at br.HeadSHA, attempt n) merged
// with every sibling that shares a directory with it, in a throwaway worktree and a gate slot.
// When the merge or the command fails it finds the clashing sibling(s), sends the task back to
// pending with findings naming them and returns reopened. It does nothing when
// merge_check_cmd is empty or there are no such siblings.
func (p *Pipeline) mergeCheck(ctx context.Context, t store.Task, br store.Branch, startSHA string, n int) (reopened bool, reason string, err error) {
	cmd := strings.TrimSpace(p.Cfg.MergeCheckCmd)
	if cmd == "" {
		return false, "", nil
	}
	sibs, err := p.mergeSiblings(ctx, t, startSHA, br.HeadSHA)
	if err != nil || len(sibs) == 0 {
		return false, "", err
	}
	ids := make([]string, len(sibs))
	for i, s := range sibs {
		ids[i] = s.id
	}
	p.logf("%s a%d merge check with %s: %s", t.ID, n, strings.Join(ids, ", "), cmd)

	release, err := p.AcquireGateSlot(ctx, store.GateHolder{TaskID: t.ID, Attempt: n}, func() {
		p.logf("%s a%d merge check waiting for a free gate slot (max_parallel_gates = %d)", t.ID, n, p.maxParallelGates())
	})
	if err != nil {
		return false, "", fmt.Errorf("merge check of %s: wait for a gate slot: %w", t.ID, err)
	}
	defer func() {
		if rerr := release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("merge check of %s: release the gate slot: %w", t.ID, rerr))
		}
	}()

	clashes, err := p.mergeCheckRun(ctx, t, br.HeadSHA, n, cmd, sibs)
	if err != nil {
		return false, "", err
	}
	if len(clashes) == 0 {
		p.logf("%s a%d merge check ok", t.ID, n)
		return false, "", nil
	}

	var findings []prompt.Finding
	var names []string
	seen := map[string]bool{}
	for _, c := range clashes {
		findings = append(findings, mergeClashFinding(c, cmd))
		for _, id := range c.siblings {
			if !seen[id] {
				seen[id] = true
				names = append(names, id)
			}
		}
	}
	reason = fmt.Sprintf("merge check failed against sibling task(s) %s", strings.Join(names, ", "))
	p.logf("%s a%d merge check FAIL: clashes with %s", t.ID, n, strings.Join(names, ", "))
	if _, err := p.Store.AppendEvent(ctx, MergeCheckFailedEvent, "", t.ID, map[string]any{
		"attempt": n, "head": br.HeadSHA, "cmd": cmd, "siblings": names, "findings": findings,
	}); err != nil {
		return false, "", fmt.Errorf("record the failed merge check of %s; check the mill database and run the task again: %w", t.ID, err)
	}
	if err := p.Store.RequestChanges(ctx, t.ID, n, findings, reason); err != nil {
		return false, "", fmt.Errorf("send %s back to pending after its merge check failed; run `mill review changes %s` by hand: %w", t.ID, t.ID, err)
	}
	return true, reason, nil
}

// mergeCheckRun merges head with every sibling in a throwaway worktree, which it always
// removes, and runs cmd there. It returns no clashes when that passes. Otherwise it merges head
// with one sibling at a time to find the clashing ones; when none clashes alone, the clash is
// reported against all of them.
func (p *Pipeline) mergeCheckRun(ctx context.Context, t store.Task, head string, n int, cmd string, sibs []mergeSibling) (clashes []mergeClash, err error) {
	wt, err := p.addMergeCheckWorktree(ctx, t, head)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rerr := p.removeMergeCheckWorktree(ctx, wt); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	logDir := filepath.Join(p.Paths.Runs, t.ID, fmt.Sprintf("a%d", n), "merge-check")

	all := make([]string, len(sibs))
	for i, s := range sibs {
		all[i] = s.id
	}
	combined, err := p.mergeAndCheck(ctx, t, wt, head, n, cmd, logDir, sibs, all)
	if err != nil || combined == nil {
		return nil, err
	}
	if len(sibs) == 1 {
		return []mergeClash{*combined}, nil
	}
	for _, s := range sibs {
		c, err := p.mergeAndCheck(ctx, t, wt, head, n, cmd, logDir, []mergeSibling{s}, []string{s.id})
		if err != nil {
			return nil, err
		}
		if c != nil {
			clashes = append(clashes, *c)
		}
	}
	if len(clashes) == 0 {
		// No sibling clashes alone: the clash needs several of them, and all are reported.
		return []mergeClash{*combined}, nil
	}
	return clashes, nil
}

// mergeAndCheck resets wt to head, merges the siblings' branches and runs cmd. It returns nil
// when everything merged and cmd passed, and the clash (named after ids) otherwise.
func (p *Pipeline) mergeAndCheck(ctx context.Context, t store.Task, wt, head string, n int, cmd, logDir string, sibs []mergeSibling, ids []string) (*mergeClash, error) {
	if _, err := gitx.Run(ctx, wt, "reset", "-q", "--hard", head); err != nil {
		return nil, fmt.Errorf("merge check of %s: reset the throwaway worktree %s to %s: %w", t.ID, wt, short(head), err)
	}
	if _, err := gitx.Run(ctx, wt, "clean", "-q", "-fdx"); err != nil {
		return nil, fmt.Errorf("merge check of %s: clean the throwaway worktree %s: %w", t.ID, wt, err)
	}
	for _, s := range sibs {
		msg := fmt.Sprintf("mill: merge check of %s with %s", t.ID, s.id)
		err := gitx.Merge(ctx, wt, s.branch, msg)
		var ce *gitx.ConflictError
		if errors.As(err, &ce) {
			return &mergeClash{siblings: ids, files: ce.Files}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("merge check of %s: merge %s (task %s) in %s: %w", t.ID, s.branch, s.id, wt, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("merge check of %s interrupted: %w", t.ID, err)
	}
	g := gate.Run(ctx, gate.Options{
		Host: p.Host, Fallback: host.NewExec(), Dir: wt, Cmds: []string{cmd}, LogDir: logDir,
		Env: p.env(nil), Timeout: p.Cfg.GateTimeout.Duration, Nice: p.Cfg.GateNice,
		Label: fmt.Sprintf("%s a%d merge check", t.ID, n),
	})
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("merge check of %s interrupted: %w", t.ID, err)
	}
	if g.Passed {
		return nil, nil
	}
	if len(g.Commands) == 0 {
		// The command did not run: the check cannot decide, so it fails.
		return &mergeClash{siblings: ids, cmd: &gate.CmdResult{Cmd: cmd, Exit: -1, Error: g.Reason}}, nil
	}
	c := g.Commands[0]
	return &mergeClash{siblings: ids, cmd: &c}, nil
}

// mergeClashFinding describes a clash for the task's next attempt.
func mergeClashFinding(c mergeClash, cmd string) prompt.Finding {
	with := "verified sibling task " + c.siblings[0]
	if len(c.siblings) > 1 {
		with = "verified sibling tasks " + strings.Join(c.siblings, ", ") + " (together; no single one clashes alone)"
	}
	if c.cmd == nil {
		return prompt.Finding{Source: "merge-check", File: strings.Join(c.files, ", "), Issue: fmt.Sprintf(
			"merging this task with %s conflicts in %s. Change this task so that it merges cleanly with %s, keeping both changes.",
			with, strings.Join(c.files, ", "), strings.Join(c.siblings, ", "))}
	}
	what := fmt.Sprintf("exit %d", c.cmd.Exit)
	if c.cmd.Error != "" {
		what = c.cmd.Error
	}
	issue := fmt.Sprintf("merged with %s, the merge check `%s` fails (%s). Change this task so that it works together with %s.",
		with, cmd, what, strings.Join(c.siblings, ", "))
	if c.cmd.Tail != "" {
		issue += " Output (last lines):\n" + indent(c.cmd.Tail)
	}
	return prompt.Finding{Source: "merge-check", Issue: issue}
}

// mergeSiblings lists the other tasks of t's goal that are verified (or published) and not
// landed, with a branch, and whose changed files share a directory with t's changes between
// startSHA and head. None for a standalone task.
func (p *Pipeline) mergeSiblings(ctx context.Context, t store.Task, startSHA, head string) ([]mergeSibling, error) {
	goal, _ := store.SplitTaskID(t.ID)
	if goal == "" {
		return nil, nil
	}
	files, err := gitx.ChangedPaths(ctx, p.Paths.Root, startSHA, head)
	if err != nil {
		return nil, fmt.Errorf("merge check of %s: list its changed files: %w", t.ID, err)
	}
	dirs := fileDirs(files)
	if len(dirs) == 0 {
		return nil, nil
	}
	tasks, err := p.Store.GoalTasks(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("merge check of %s: list the tasks of goal %s: %w", t.ID, goal, err)
	}
	var out []mergeSibling
	for _, s := range tasks {
		if s.ID == t.ID || (s.Status != store.TaskVerified && s.Status != store.TaskPublished) {
			continue
		}
		sb, err := p.Store.GetBranch(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("merge check of %s: read the branch of verified sibling %s: %w", t.ID, s.ID, err)
		}
		from := sb.StartSHA
		if from == "" {
			from = sb.BaseSHA
		}
		sfiles, err := gitx.ChangedPaths(ctx, p.Paths.Root, from, sb.HeadSHA)
		if err != nil {
			return nil, fmt.Errorf("merge check of %s: list the changed files of sibling %s: %w", t.ID, s.ID, err)
		}
		for d := range fileDirs(sfiles) {
			if dirs[d] {
				out = append(out, mergeSibling{id: s.ID, branch: sb.Name})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// fileDirs is the set of directories holding files ("." for the repository root).
func fileDirs(files []string) map[string]bool {
	dirs := map[string]bool{}
	for _, f := range files {
		dirs[path.Dir(f)] = true
	}
	return dirs
}

// addMergeCheckWorktree creates a detached throwaway worktree at head under the worktree root.
func (p *Pipeline) addMergeCheckWorktree(ctx context.Context, t store.Task, head string) (string, error) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	parent := filepath.Join(p.Cfg.Worktrees(p.Paths.Root, p.Home), ".merge-check")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("merge check of %s: create %s: %w", t.ID, parent, err)
	}
	wt, err := os.MkdirTemp(parent, strings.ReplaceAll(t.ID, "/", "-")+"-")
	if err != nil {
		return "", fmt.Errorf("merge check of %s: create a throwaway worktree directory in %s: %w", t.ID, parent, err)
	}
	if err := gitx.AddDetachedWorktree(ctx, p.Paths.Root, wt, head); err != nil {
		if rerr := os.RemoveAll(wt); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove %s by hand: %w", wt, rerr))
		}
		return "", fmt.Errorf("merge check of %s: add a throwaway worktree at %s: %w", t.ID, short(head), err)
	}
	return wt, nil
}

// removeMergeCheckWorktree removes the throwaway worktree, even when ctx is done.
func (p *Pipeline) removeMergeCheckWorktree(ctx context.Context, wt string) error {
	ctx = context.WithoutCancel(ctx)
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	var errs []error
	if err := gitx.RemoveWorktree(ctx, p.Paths.Root, wt); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(wt); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("removing the merge check worktree %s failed; remove it by hand (git worktree remove --force %s): %w", wt, wt, errors.Join(errs...))
}
