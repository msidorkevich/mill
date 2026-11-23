// Package cleanup removes what a landed task leaves behind: its worktree, its local branch and,
// optionally, its review branch on the remote. `mill clean` and the syncer share it.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

// Options configure the cleanup of one task.
type Options struct {
	Store *store.Store
	Root  string // the repository root
	// Remote names the git remote holding review branches; with DeleteRemote the task's
	// review branch is deleted there while it is still at the published commit.
	Remote       string
	DeleteRemote bool
	// DryRun reports what would be done and changes nothing.
	DryRun bool
	// KeepDirty keeps a worktree with uncommitted changes, and so the task's branch, and
	// reports it in Result.Kept.
	KeepDirty bool
	// Out receives one line per action, in the format of `mill clean`; nil discards.
	Out io.Writer
}

// Skip is a step that was not taken.
type Skip struct {
	Why string
	// Expected marks a skip that needs no attention: nothing was published, or the forge
	// already deleted the review branch.
	Expected bool
}

// Failure is a step that failed.
type Failure struct {
	Step string
	Err  error
}

// Result is what cleaning one task did (or, with DryRun, would do).
type Result struct {
	TaskID       string
	Worktree     string // the worktree removed, "" if none
	Branch       string // the local branch deleted, "" if none
	RemoteBranch string // the review branch deleted on the remote, "" if none
	// Kept explains why the worktree and branch were kept (KeepDirty); "" otherwise.
	Kept     string
	Skipped  []Skip
	Failures []Failure
}

// Cleaned reports whether anything was removed.
func (r Result) Cleaned() bool { return r.Worktree != "" || r.Branch != "" || r.RemoteBranch != "" }

// Task cleans up one landed task. Step failures are collected in the result; an error means
// the task's branch record could not be read.
func Task(ctx context.Context, o Options, id string) (Result, error) {
	r := Result{TaskID: id}
	b, err := o.Store.GetBranch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		r.Skipped = append(r.Skipped, Skip{Why: "no branch recorded"})
		return r, nil
	}
	if err != nil {
		return r, err
	}
	verb := func(done, would string) string {
		if o.DryRun {
			return would
		}
		return done
	}
	say := func(format string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, format, a...)
		}
	}
	step := func(err error, what string) bool {
		if err != nil {
			r.Failures = append(r.Failures, Failure{Step: what, Err: err})
			return false
		}
		return true
	}
	changed := false
	ok := true

	if b.Worktree != "" {
		if _, statErr := os.Stat(b.Worktree); statErr == nil {
			if o.KeepDirty {
				dirty, err := gitx.HasChanges(ctx, b.Worktree)
				if !step(err, "check worktree "+b.Worktree+" for uncommitted changes") {
					return r, nil
				}
				if dirty {
					r.Kept = fmt.Sprintf("worktree %s has uncommitted changes; kept it and branch %s. Commit or discard them, then run `mill clean`", b.Worktree, b.Name)
					return r, nil
				}
			}
			say("%s  %s worktree %s\n", id, verb("removed", "would remove"), b.Worktree)
			if !o.DryRun {
				ok = step(gitx.RemoveWorktree(ctx, o.Root, b.Worktree), "remove worktree")
			}
			if ok {
				r.Worktree = b.Worktree
			}
		}
		changed = true
	}
	if ok && !o.DryRun {
		ok = step(gitx.PruneWorktrees(ctx, o.Root), "prune worktrees")
	}
	if ok && gitx.BranchExists(ctx, o.Root, b.Name) {
		say("%s  %s branch %s\n", id, verb("deleted", "would delete"), b.Name)
		if !o.DryRun {
			ok = step(gitx.DeleteBranch(ctx, o.Root, b.Name), "delete branch")
		}
		if ok {
			r.Branch = b.Name
			changed = true
		}
	}
	if ok && changed && !o.DryRun {
		ok = step(o.Store.MarkTaskCleaned(ctx, id, map[string]string{"branch": b.Name, "worktree": b.Worktree}), "record cleanup")
	}
	if ok && o.DeleteRemote {
		remote(ctx, o, b, &r, say, step)
	}
	return r, nil
}

// remote deletes a landed task's review branch when it is still at the published sha.
func remote(ctx context.Context, o Options, b store.Branch, r *Result, say func(string, ...any), step func(error, string) bool) {
	ref := b.PublishedRef
	if ref == "" || b.PublishedSHA == "" {
		r.Skipped = append(r.Skipped, Skip{Why: "never published; no remote branch to delete", Expected: true})
		return
	}
	sha, exists, err := gitx.RemoteBranchSHA(ctx, o.Root, o.Remote, ref)
	if !step(err, "look up "+ref+" on "+o.Remote) {
		return
	}
	switch {
	case !exists:
		r.Skipped = append(r.Skipped, Skip{Why: ref + " already deleted on " + o.Remote, Expected: true})
	case sha != b.PublishedSHA:
		r.Skipped = append(r.Skipped, Skip{Why: ref + " moved on " + o.Remote + " (now " + short(sha) + ", published " + short(b.PublishedSHA) + "); left alone"})
	case o.DryRun:
		say("%s  would delete %s/%s\n", r.TaskID, o.Remote, ref)
		r.RemoteBranch = ref
	default:
		if step(gitx.DeleteReview(ctx, o.Root, o.Remote, ref, b.PublishedSHA), "delete "+ref+" on "+o.Remote) {
			say("%s  deleted %s/%s\n", r.TaskID, o.Remote, ref)
			r.RemoteBranch = ref
		}
	}
}

// PruneEmptyDirs removes (or with dry, reports on out) every empty directory under top,
// bottom-up, keeping top itself, and returns how many it removed. A missing top is not an
// error. Symlinks are never followed: a symlink counts as content. A directory holding a .git
// entry (file or directory) is a git worktree or checkout: it counts as content and is never
// descended into.
func PruneEmptyDirs(out io.Writer, top string, dry bool) (int, error) {
	if _, err := os.Lstat(top); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("inspect worktree root %s: %w", top, err)
	}
	n := 0
	if _, err := pruneEmptyDirs(out, top, top, dry, &n); err != nil {
		return n, fmt.Errorf("remove empty directories under %s: %w", top, err)
	}
	return n, nil
}

func pruneEmptyDirs(out io.Writer, top, dir string, dry bool, n *int) (bool, error) {
	if dir != top {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	empty := true
	for _, e := range entries {
		if !e.IsDir() { // DirEntry type comes from lstat: symlinks are not dirs
			empty = false
			continue
		}
		sub := filepath.Join(dir, e.Name())
		subEmpty, err := pruneEmptyDirs(out, top, sub, dry, n)
		if err != nil {
			return false, err
		}
		if !subEmpty {
			empty = false
			continue
		}
		if dry {
			if out != nil {
				fmt.Fprintf(out, "would remove empty directory %s\n", sub)
			}
		} else {
			if err := os.Remove(sub); err != nil {
				return false, err
			}
			if out != nil {
				fmt.Fprintf(out, "removed empty directory %s\n", sub)
			}
		}
		*n++
	}
	return empty, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
