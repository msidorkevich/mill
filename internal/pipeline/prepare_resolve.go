package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
)

// stoppedError is returned by prepare when an agent run during preparation had to stop
// (budget, rate limit, interruption, timeout); the run loop blocks the task with the stop.
type stoppedError struct {
	stop        string
	rateLimited bool
	resetsAt    time.Time
}

func (e *stoppedError) Error() string { return e.stop }

// mergeDependency merges dependency m into the new worktree wt of branch name. Conflicts that
// rerere does not resolve go to the resolver agent. It returns nil when the merge is committed.
// Otherwise it aborts the merge and removes the worktree and branch, and returns a
// *blockedError (the conflict could not be resolved), a *stoppedError (the resolver had to
// stop) or mill's own error joined with any cleanup error.
func (p *Pipeline) mergeDependency(ctx context.Context, t store.Task, wt, name string, m mergeDep) error {
	ms, err := gitx.MergeStart(ctx, wt, m.rev, m.message, true)
	if err != nil {
		return errors.Join(fmt.Errorf("merge dependency %s: %w", m.id, err), p.abortCreated(ctx, wt, name))
	}
	if ms.Done {
		if ms.RerereResolved {
			p.logf("%s: rerere resolved the conflict with dependency %s", t.ID, m.id)
		}
		return nil
	}
	files := ms.Files
	conflict := fmt.Sprintf("dependency %s (branch %s) conflicts with the other dependencies in %s; resolve the conflict in the plan or by hand",
		m.id, m.branch, strings.Join(files, ", "))
	in, err := p.prepareResolverInput(ctx, t, wt, m, files)
	if err != nil {
		return errors.Join(fmt.Errorf("%s: %w", conflict, err), p.abortCreated(ctx, wt, name))
	}
	p.logf("%s: dependency %s conflicts in %s; starting the resolver", t.ID, m.id, strings.Join(files, ", "))
	out, err := p.Resolve(ctx, t.ID, in, wt)
	if err != nil {
		return errors.Join(fmt.Errorf("resolve the conflict with dependency %s: %w", m.id, err), p.abortCreated(ctx, wt, name))
	}
	if out.Stop != "" {
		if cerr := p.abortCreated(ctx, wt, name); cerr != nil {
			return fmt.Errorf("the resolver stopped (%s): %w", out.Stop, cerr)
		}
		return &stoppedError{stop: out.Stop, rateLimited: out.RateLimited, resetsAt: out.ResetsAt}
	}
	failed := out.Reason
	if failed == "" {
		marked, err := gitx.HasConflictMarkers(ctx, wt, files)
		if err != nil {
			return errors.Join(fmt.Errorf("%s: %w", conflict, err), p.abortCreated(ctx, wt, name))
		}
		if len(marked) > 0 {
			failed = fmt.Sprintf("conflict markers remain in %s (the resolver said: %s)", strings.Join(marked, ", "), oneLine(out.Output.Summary))
		} else if _, err := gitx.MergeContinue(ctx, wt, files, m.message); err != nil {
			failed = fmt.Sprintf("committing the merge failed: %v (the resolver said: %s)", err, oneLine(out.Output.Summary))
		}
	}
	if failed != "" {
		reason := conflict + "; the resolver failed: " + failed
		if cerr := p.abortCreated(ctx, wt, name); cerr != nil {
			return fmt.Errorf("%s: %w", reason, cerr)
		}
		return &blockedError{reason}
	}
	p.logf("%s: resolver resolved the conflict with dependency %s in %s", t.ID, m.id, strings.Join(files, ", "))
	return nil
}

// prepareResolverInput builds the resolver's input for a stopped merge of m into wt: the
// conflicted files' hunks and the commits on both sides since their merge base.
func (p *Pipeline) prepareResolverInput(ctx context.Context, t store.Task, wt string, m mergeDep, files []string) (prompt.ResolverInput, error) {
	in := prompt.ResolverInput{Task: t.Spec, Merging: fmt.Sprintf("%s (branch %s)", m.id, m.branch)}
	for _, f := range files {
		h, err := gitx.ConflictHunks(ctx, wt, f)
		if err != nil {
			return in, fmt.Errorf("read the conflict in %s: %w", f, err)
		}
		in.Conflicts = append(in.Conflicts, prompt.ConflictFile{Path: f, Hunks: h})
	}
	base, err := gitx.Run(ctx, wt, "merge-base", "HEAD", m.rev)
	if err != nil {
		return in, fmt.Errorf("find the merge base of HEAD and %s: %w", m.branch, err)
	}
	if in.BaseCommits, err = commitsBetween(ctx, wt, base, "HEAD"); err != nil {
		return in, err
	}
	if in.OtherCommits, err = commitsBetween(ctx, wt, base, m.rev); err != nil {
		return in, err
	}
	return in, nil
}

// commitsBetween summarises the commits in from..to for the resolver's prompt.
func commitsBetween(ctx context.Context, dir, from, to string) ([]prompt.BaseCommit, error) {
	sums, err := gitx.CommitSummaries(ctx, dir, from, to)
	if err != nil {
		return nil, err
	}
	var out []prompt.BaseCommit
	for _, c := range sums {
		out = append(out, prompt.BaseCommit{SHA: c.SHA, Subject: c.Subject, Trailers: strings.Join(c.Trailers, "\n")})
	}
	return out, nil
}

// abortCreated aborts the merge in progress in wt, if any, and removes the worktree and branch
// this preparation created. The abort failing matters only when the removal fails too, since
// removing the worktree also drops its merge state.
func (p *Pipeline) abortCreated(ctx context.Context, wt, name string) error {
	ctx = context.WithoutCancel(ctx)
	var abortErr error
	if _, err := gitx.Run(ctx, wt, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		abortErr = gitx.MergeAbort(ctx, wt)
	}
	if err := p.removeCreated(ctx, wt, name); err != nil {
		return errors.Join(abortErr, err)
	}
	return nil
}
