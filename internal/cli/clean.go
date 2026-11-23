package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/msidorkevich/mill/internal/cleanup"
	"github.com/msidorkevich/mill/internal/store"
)

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
	o := cleanup.Options{Store: s, Root: paths.Root, Remote: cfg.Remote, DeleteRemote: *remote, DryRun: *dry, Out: env.Stdout}
	var worktrees, branches, remotes, failures int
	var skipped []string
	for _, t := range tasks {
		r, err := cleanup.Task(ctx, o, t.ID)
		if err != nil {
			return fail(err)
		}
		for _, f := range r.Failures {
			failures++
			fmt.Fprintf(env.Stderr, "mill clean: %s: %s: %v\n", t.ID, f.Step, f.Err)
		}
		for _, sk := range r.Skipped {
			skipped = append(skipped, t.ID+": "+sk.Why)
		}
		if r.Worktree != "" {
			worktrees++
		}
		if r.Branch != "" {
			branches++
		}
		if r.RemoteBranch != "" {
			remotes++
		}
	}

	home := env.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return fail(fmt.Errorf("find home directory: %w", err))
		}
	}
	dirs, err := cleanup.PruneEmptyDirs(env.Stdout, cfg.Worktrees(paths.Root, home), *dry)
	if err != nil {
		failures++
		fmt.Fprintf(env.Stderr, "mill clean: %v\n", err)
	}

	fmt.Fprintf(env.Stdout, "\n%s: %d worktree(s), %d branch(es), %d remote branch(es), %d empty director(ies).\n",
		verb("Cleaned", "Would clean"), worktrees, branches, remotes, dirs)
	if len(skipped) > 0 {
		fmt.Fprintln(env.Stdout, "Skipped:")
		for _, sk := range skipped {
			fmt.Fprintf(env.Stdout, "  %s\n", sk)
		}
	}
	if failures > 0 {
		fmt.Fprintf(env.Stderr, "mill clean: %d step(s) failed; fix the cause and run `mill clean` again.\n", failures)
		return ExitError
	}
	return ExitOK
}
