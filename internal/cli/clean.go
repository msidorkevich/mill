package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

// cleanCounts is what one `mill clean` did (or, with --dry-run, would do).
type cleanCounts struct {
	worktrees, branches, remote int
	dirs                        int
	skipped                     []string
}

func runClean(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "clean")
	remote := fs.Bool("remote", false, "also delete review/* branches on the remote that are still at the published commit")
	dry := fs.Bool("dry-run", false, "print what would be removed and change nothing")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill clean: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	release, err := lockAndRecover(ctx, env, "clean", paths, s, cfg.Prices)
	if err != nil {
		return fail(err)
	}
	defer release()

	tasks, err := s.TaskSummaries(ctx, store.TaskLanded)
	if err != nil {
		return fail(err)
	}
	verb := func(done, would string) string {
		if *dry {
			return would
		}
		return done
	}
	var c cleanCounts
	var failures int
	skip := func(id, why string) {
		c.skipped = append(c.skipped, id+": "+why)
	}
	for _, t := range tasks {
		b, err := s.GetBranch(ctx, t.ID)
		if errors.Is(err, store.ErrNotFound) {
			skip(t.ID, "no branch recorded")
			continue
		}
		if err != nil {
			return fail(err)
		}
		changed := false
		step := func(err error, what string) bool {
			if err != nil {
				failures++
				fmt.Fprintf(env.Stderr, "mill clean: %s: %s: %v\n", t.ID, what, err)
				return false
			}
			return true
		}
		ok := true

		if b.Worktree != "" {
			if _, statErr := os.Stat(b.Worktree); statErr == nil {
				fmt.Fprintf(env.Stdout, "%s  %s worktree %s\n", t.ID, verb("removed", "would remove"), b.Worktree)
				if !*dry {
					ok = step(gitx.RemoveWorktree(ctx, paths.Root, b.Worktree), "remove worktree")
				}
				if ok {
					c.worktrees++
				}
			}
			changed = true
		}
		if ok && !*dry {
			ok = step(gitx.PruneWorktrees(ctx, paths.Root), "prune worktrees")
		}
		if ok && gitx.BranchExists(ctx, paths.Root, b.Name) {
			fmt.Fprintf(env.Stdout, "%s  %s branch %s\n", t.ID, verb("deleted", "would delete"), b.Name)
			if !*dry {
				ok = step(gitx.DeleteBranch(ctx, paths.Root, b.Name), "delete branch")
			}
			if ok {
				c.branches++
				changed = true
			}
		}
		if ok && changed && !*dry {
			ok = step(s.MarkTaskCleaned(ctx, t.ID, map[string]string{"branch": b.Name, "worktree": b.Worktree}), "record cleanup")
		}
		if ok && *remote {
			cleanRemote(ctx, env, paths.Root, cfg.Remote, t.ID, b, *dry, &c, skip, step)
		}
	}

	home := env.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return fail(fmt.Errorf("find home directory: %w", err))
		}
	}
	wtRoot := cfg.Worktrees(paths.Root, home)
	if _, err := os.Lstat(wtRoot); err == nil {
		if _, err := pruneEmptyDirs(env, wtRoot, wtRoot, *dry, &c); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "mill clean: remove empty directories under %s: %v\n", wtRoot, err)
		}
	} else if !os.IsNotExist(err) {
		failures++
		fmt.Fprintf(env.Stderr, "mill clean: inspect worktree root %s: %v\n", wtRoot, err)
	}

	fmt.Fprintf(env.Stdout, "\n%s: %d worktree(s), %d branch(es), %d remote branch(es), %d empty director(ies).\n",
		verb("Cleaned", "Would clean"), c.worktrees, c.branches, c.remote, c.dirs)
	if len(c.skipped) > 0 {
		fmt.Fprintln(env.Stdout, "Skipped:")
		for _, sk := range c.skipped {
			fmt.Fprintf(env.Stdout, "  %s\n", sk)
		}
	}
	if failures > 0 {
		fmt.Fprintf(env.Stderr, "mill clean: %d step(s) failed; fix the cause and run `mill clean` again.\n", failures)
		return ExitError
	}
	return ExitOK
}

// pruneEmptyDirs removes (or with dry, reports) every empty directory under dir, bottom-up,
// keeping top itself. It reports whether dir is empty afterwards (or would be, when dry).
// Symlinks are never followed: a symlink counts as content.
// A directory holding a .git entry (file or directory) is a git worktree or checkout: it counts
// as content and is never descended into.
func pruneEmptyDirs(env Env, top, dir string, dry bool, c *cleanCounts) (bool, error) {
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
		subEmpty, err := pruneEmptyDirs(env, top, sub, dry, c)
		if err != nil {
			return false, err
		}
		if !subEmpty {
			empty = false
			continue
		}
		if dry {
			fmt.Fprintf(env.Stdout, "would remove empty directory %s\n", sub)
		} else {
			if err := os.Remove(sub); err != nil {
				return false, err
			}
			fmt.Fprintf(env.Stdout, "removed empty directory %s\n", sub)
		}
		c.dirs++
	}
	return empty, nil
}

// cleanRemote deletes a landed task's review branch when it is still at the published sha.
func cleanRemote(ctx context.Context, env Env, root, remote, id string, b store.Branch, dry bool, c *cleanCounts,
	skip func(id, why string), step func(err error, what string) bool) {
	ref := b.PublishedRef
	if ref == "" || b.PublishedSHA == "" {
		skip(id, "never published; no remote branch to delete")
		return
	}
	sha, exists, err := gitx.RemoteBranchSHA(ctx, root, remote, ref)
	if !step(err, "look up "+ref+" on "+remote) {
		return
	}
	switch {
	case !exists:
		skip(id, ref+" already deleted on "+remote)
	case sha != b.PublishedSHA:
		skip(id, ref+" moved on "+remote+" (now "+short(sha)+", published "+short(b.PublishedSHA)+"); left alone")
	case dry:
		fmt.Fprintf(env.Stdout, "%s  would delete %s/%s\n", id, remote, ref)
		c.remote++
	default:
		if step(gitx.DeleteReview(ctx, root, remote, ref, b.PublishedSHA), "delete "+ref+" on "+remote) {
			fmt.Fprintf(env.Stdout, "%s  deleted %s/%s\n", id, remote, ref)
			c.remote++
		}
	}
}
