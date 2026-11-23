package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/msidorkevich/mill/internal/scheduler"
	"github.com/msidorkevich/mill/internal/store"
)

// lockedWriter makes whole-line writes from several goroutines safe.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func runRun(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "run")
	retry := fs.Bool("retry-blocked", false, "put blocked and failed tasks back to pending first")
	waitLimit := fs.Bool("wait-on-limit", false, "when a usage limit is reached, wait for it to reset and continue")
	hostFlag := hostFlagVar(fs)
	id, code, ok := goalArg(env, "run", "mill run <goal> [--retry-blocked] [--wait-on-limit] [--host auto|exec|herdr]", fs, args)
	if !ok {
		return code
	}
	if !validHostFlag(env, "run", *hostFlag) {
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill run: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	if cfg.StartChildrenOn == "published" {
		return fail(errors.New(`start_children_on = "published" is not supported yet (publishing does not exist); set start_children_on = "verified" in .mill/config.toml`))
	}

	release, err := lockAndRecover(ctx, env, "run", paths, s, cfg.Prices)
	if err != nil {
		return fail(err)
	}
	defer release()

	out := &lockedWriter{w: env.Stdout}
	p, err := newPipeline(env, "run", paths, cfg, *hostFlag, s, out)
	if err != nil {
		return fail(err)
	}
	sch := &scheduler.Scheduler{Store: s, MaxParallel: cfg.MaxParallel, RunTask: p.Run, Out: out}
	res, err := sch.RunGoal(ctx, id, scheduler.Options{RetryBlocked: *retry, WaitOnLimit: *waitLimit})
	if err != nil {
		return fail(err)
	}

	// The run may have been interrupted: the summary must still be read.
	v, err := loadGoalView(context.WithoutCancel(ctx), s, id)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(env.Stdout)
	printGoalView(env.Stdout, v)
	fmt.Fprintln(env.Stdout)
	if res.Status == store.GoalDone {
		fmt.Fprintf(env.Stdout, "Done: all tasks are verified, on branches mill/%s/*.\n", id)
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "Blocked: %s\n", res.Reason)
	if strings.HasPrefix(res.Reason, "rate limited:") {
		fmt.Fprintf(env.Stdout, "Next: after the usage limit resets, run `mill run %s --retry-blocked` (or add --wait-on-limit to wait automatically).\n", id)
		return ExitNotVerified
	}
	fmt.Fprintf(env.Stdout, "Next: inspect with `mill task show <id>`, fix the cause, then run `mill run %s --retry-blocked`.\n", id)
	return ExitNotVerified
}
