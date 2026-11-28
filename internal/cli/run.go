package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/msidorkevich/mill/internal/price"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
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

const runUsage = "mill run [<goal>] [--retry-blocked] [--wait-on-limit] [--host auto|exec|herdr|tmux]"

func runRun(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "run")
	retry := fs.Bool("retry-blocked", false, "put blocked and failed tasks back to pending first")
	waitLimit := fs.Bool("wait-on-limit", false, "when a usage limit is reached, wait for it to reset and continue")
	hostFlag := hostFlagVar(fs)
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flagErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() > 0 {
		fmt.Fprintln(env.Stderr, "Usage: "+runUsage)
		return ExitUsage
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

	release, code := acquireScheduler(ctx, env, paths, s, cfg.Prices, cfg.Host.TmuxBin)
	if release == nil {
		return code
	}
	defer release()

	out := &lockedWriter{w: env.Stdout}
	p, err := newPipeline(env, "run", paths, cfg, *hostFlag, s, out)
	if err != nil {
		return fail(err)
	}
	// Agents run up to max_parallel at once; their gates take turns in max_parallel_gates slots
	// and hold no agent slot. At most max_in_flight tasks are started and unfinished.
	p.MaxParallelGates = cfg.MaxParallelGates
	sch := &scheduler.Scheduler{Store: s, MaxParallel: cfg.MaxParallel, MaxInFlight: cfg.MaxInFlight, RunTask: p.Run, Out: out}
	res, err := sch.Run(ctx, scheduler.RunOptions{
		Options: scheduler.Options{RetryBlocked: *retry, WaitOnLimit: *waitLimit,
			// A retried task whose dependencies moved is first moved onto them, as
			// `mill task rebase` does.
			Reprepare: func(ctx context.Context, t store.Task) error {
				_, err := moveTaskOntoBase(ctx, s, cfg, paths, t)
				return err
			},
			RepoDir: paths.Root, BaseBranch: cfg.BaseBranch},
		Goal:         id,
		PollInterval: env.SchedulerPoll,
	})
	if err != nil {
		return fail(err)
	}
	if len(res.Goals) == 0 && len(res.Tasks) == 0 {
		fmt.Fprintln(env.Stdout, "Nothing to run: no approved goal or queued task is ready.")
		return ExitOK
	}

	// The run may have been interrupted: the summary must still be read.
	bg := context.WithoutCancel(ctx)
	code = ExitOK
	for _, g := range res.Goals {
		v, err := loadGoalView(bg, s, g.GoalID)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(env.Stdout)
		printGoalView(env.Stdout, v)
		fmt.Fprintln(env.Stdout)
		if g.Status == store.GoalDone {
			fmt.Fprintf(env.Stdout, "Done: all tasks are verified, on branches mill/%s/*.\n", g.GoalID)
			continue
		}
		code = ExitNotVerified
		fmt.Fprintf(env.Stdout, "Blocked: %s\n", g.Reason)
		if strings.HasPrefix(g.Reason, "rate limited:") {
			fmt.Fprintf(env.Stdout, "Next: after the usage limit resets, run `mill run %s --retry-blocked` (or add --wait-on-limit to wait automatically).\n", g.GoalID)
			continue
		}
		fmt.Fprintf(env.Stdout, "Next: inspect with `mill task show <id>`, fix the cause, then run `mill run %s --retry-blocked`.\n", g.GoalID)
	}
	if len(res.Tasks) > 0 {
		fmt.Fprintln(env.Stdout)
	}
	for _, t := range res.Tasks {
		line := fmt.Sprintf("task %s: %s", t.TaskID, t.Status)
		if t.Reason != "" {
			line += " (" + t.Reason + ")"
		}
		fmt.Fprintln(env.Stdout, line)
		if t.Status != store.TaskVerified {
			code = ExitNotVerified
		}
	}
	return code
}

// acquireScheduler makes this process the repository's single scheduler: it takes the run
// lock, then repairs what a killed run left behind. When it fails it prints why and returns a
// nil release with the exit code; otherwise call release when the run ends.
func acquireScheduler(ctx context.Context, env Env, paths project.Paths, s *store.Store, prices map[string]price.Rates, tmuxBin string) (release func(), code int) {
	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		if pid, held, herr := runlock.Held(paths.RunLock); herr == nil && held {
			fmt.Fprintf(env.Stderr, "mill run: a scheduler is already running for this repository (pid %d); it picks up newly approved goals and queued tasks, follow it with `mill watch`\n", pid)
		} else {
			fmt.Fprintf(env.Stderr, "mill run: %v\n", err)
		}
		return nil, ExitError
	}
	release = func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "mill run: %v\n", err)
		}
	}
	fail := func(err error) (func(), int) {
		release()
		fmt.Fprintf(env.Stderr, "mill run: %v\n", err)
		return nil, ExitError
	}
	if err := killLeftoverWorkers(ctx, env.Stdout, s, hostWindows{runs: paths.Runs, lookPath: env.lookPath, tmuxBin: tmuxBin}); err != nil {
		return fail(err)
	}
	usage, err := interruptedUsage(ctx, env.Stdout, s, prices)
	if err != nil {
		return fail(err)
	}
	tasks, attempts, err := s.RecoverInterrupted(ctx, usage)
	if err != nil {
		return fail(err)
	}
	if tasks > 0 || attempts > 0 {
		fmt.Fprintf(env.Stdout, "recovered %d interrupted task(s) and %d running attempt(s) from a previous run\n", tasks, attempts)
	}
	return release, ExitOK
}
