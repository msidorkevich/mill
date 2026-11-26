package pipeline

import (
	"context"
	"fmt"
	"slices"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
)

// BaseChanges are the changes made on a task's base since the task started that concern the
// task.
type BaseChanges struct {
	Base  string   // the branch the task is based on
	From  string   // the base commit the task started from
	To    string   // the current tip of the base
	Files []string // changed in From..To and owned or changed by the task; sorted, unique
	Stat  string   // `git diff --stat` of From..To limited to Files; empty without Files
}

// FindBaseChanges lists the files changed on t's base since br started that match t's owns or
// that the task itself changed. An error means it could not decide.
func FindBaseChanges(ctx context.Context, root string, cfg config.Config, t store.Task, br store.Branch) (BaseChanges, error) {
	from, start := br.BaseSHA, br.StartSHA
	if from == "" {
		from = br.StartSHA
	}
	if start == "" {
		start = br.BaseSHA
	}
	res := BaseChanges{Base: br.Base, From: from}
	if br.Base == "" || from == "" {
		return BaseChanges{}, fmt.Errorf("find the base changes of task %s: its branch record has no base branch or no base commit; prepare the task again", t.ID)
	}
	to, err := currentBaseTip(ctx, root, cfg, br.Base)
	if err != nil {
		return BaseChanges{}, fmt.Errorf("find the base changes of task %s since %s: resolve the tip of %s; restore or fetch that branch and retry: %w", t.ID, short(from), br.Base, err)
	}
	res.To = to
	if from == to {
		return res, nil
	}
	changed, err := gitx.ChangedPaths(ctx, root, from, to)
	if err != nil {
		return BaseChanges{}, fmt.Errorf("list the files changed on %s in %s..%s; check that both commits exist in %s (fetch them if not) and retry: %w", br.Base, from, to, root, err)
	}
	if br.HeadSHA == "" {
		return BaseChanges{}, fmt.Errorf("find the base changes of task %s: its branch record has no head commit, so the files it changed are unknown; prepare the task again", t.ID)
	}
	own, err := gitx.ChangedPaths(ctx, root, start, br.HeadSHA)
	if err != nil {
		return BaseChanges{}, fmt.Errorf("list the files task %s changed in %s..%s; check that both commits exist in %s (restore branch %s if not) and retry: %w", t.ID, start, br.HeadSHA, root, br.Name, err)
	}
	// changed is sorted and unique, so the files kept from it are too.
	for _, f := range changed {
		owned := slices.ContainsFunc(t.Owns, func(o string) bool { return plan.MatchOwns(o, f) })
		if owned || slices.Contains(own, f) {
			res.Files = append(res.Files, f)
		}
	}
	if len(res.Files) == 0 {
		return res, nil
	}
	// Literal pathspecs: a file name containing `*` or `[` must match only itself.
	args := append([]string{"--literal-pathspecs", "diff", "--stat", "--no-renames", "--no-color", "--no-ext-diff", from, to, "--"}, res.Files...)
	if res.Stat, err = gitx.Run(ctx, root, args...); err != nil {
		return BaseChanges{}, fmt.Errorf("get the diff stat of %s..%s; check that both commits exist in %s (fetch them if not) and retry: %w", from, to, root, err)
	}
	return res, nil
}

// currentBaseTip returns the commit base points to now: the remote-tracking branch when base
// is the configured base branch and that ref exists, otherwise the local branch.
func currentBaseTip(ctx context.Context, root string, cfg config.Config, base string) (string, error) {
	if base == cfg.BaseBranch && cfg.Remote != "" {
		ref := "refs/remotes/" + cfg.Remote + "/" + base
		// for-each-ref succeeds with no output for a missing ref, so a git failure is not
		// mistaken for one.
		out, err := gitx.Run(ctx, root, "for-each-ref", "--format=%(refname)", ref)
		if err != nil {
			return "", err
		}
		if out == ref {
			return gitx.RevParse(ctx, root, ref)
		}
	}
	return gitx.RevParse(ctx, root, "refs/heads/"+base)
}
