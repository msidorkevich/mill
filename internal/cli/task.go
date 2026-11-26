package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/price"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/scheduler"
	"github.com/msidorkevich/mill/internal/skiptest"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// ExitNotVerified means the task ran but ended blocked or failed.
const ExitNotVerified = 4

func runTask(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		w := env.Stderr
		code := ExitUsage
		if len(args) > 0 {
			w, code = env.Stdout, ExitOK
		}
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  mill task run --file task.json [--retry]   run one task until verified, blocked or failed")
		fmt.Fprintln(w, "  mill task show <id> [--json]               show a task, its attempts, spend and findings")
		fmt.Fprintln(w, "  mill task list [--status S] [--all] [--json]  list current work first, then the 20 newest others (--all: every task, oldest first)")
		fmt.Fprintln(w, "  mill task edit <id> [--file spec.json]     change the spec of a pending, blocked or failed task (opens $EDITOR without --file)")
		fmt.Fprintln(w, "  mill task rebase <id>                      move a pending, blocked or failed task's own commits onto its base's current tip")
		return code
	}
	switch args[0] {
	case "run":
		return runTaskRun(ctx, env, args[1:])
	case "show":
		return runTaskShow(ctx, env, args[1:])
	case "list":
		return runTaskList(ctx, env, args[1:])
	case "edit":
		return runTaskEdit(ctx, env, args[1:])
	case "rebase":
		return runTaskRebase(ctx, env, args[1:])
	}
	fmt.Fprintf(env.Stderr, "mill task: unknown subcommand %q\n", args[0])
	return ExitUsage
}

// openProject finds the repository, loads its config and opens its store.
func openProject(ctx context.Context, env Env) (project.Paths, config.Config, *store.Store, error) {
	paths, err := project.FindContext(ctx, env.Dir)
	if err != nil {
		return paths, config.Config{}, nil, err
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return paths, cfg, nil, err
	}
	s, err := store.Open(ctx, paths.DB, store.Options{})
	if err != nil {
		return paths, cfg, nil, err
	}
	return paths, cfg, s, nil
}

func runTaskRun(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "task run")
	file := fs.String("file", "", "task file (JSON)")
	retry := fs.Bool("retry", false, "run a blocked or failed task again, with the spec from --file")
	hostFlag := hostFlagVar(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if !validHostFlag(env, "task run", *hostFlag) {
		return ExitUsage
	}
	if *file == "" {
		fmt.Fprintln(env.Stderr, "mill task run: --file is required")
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(env.Dir, path)
	}
	spec, err := taskspec.Load(path)
	if err != nil {
		return fail(err)
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	// A live scheduler owns the repository: queue the task for it and wait for the result.
	pid, held, err := runlock.Held(paths.RunLock)
	if err != nil {
		return fail(err)
	}
	if held {
		return joinScheduler(ctx, env, paths, cfg, *hostFlag, s, spec, *retry, pid)
	}

	release, err := lockAndRecover(ctx, env, "task run", paths, s, cfg.Prices)
	if err != nil {
		// A scheduler may have started between the probe and taking the lock.
		if pid, held, herr := runlock.Held(paths.RunLock); herr == nil && held {
			return joinScheduler(ctx, env, paths, cfg, *hostFlag, s, spec, *retry, pid)
		}
		return fail(err)
	}
	defer release()

	existing, err := s.GetTask(ctx, spec.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if *retry {
			return fail(fmt.Errorf("task %s does not exist yet; run it without --retry", spec.ID))
		}
		if err := s.CreateTask(ctx, spec); err != nil {
			return fail(err)
		}
	case err != nil:
		return fail(err)
	case !*retry:
		return fail(fmt.Errorf("task %s already exists (status %s); use --retry to run it again", spec.ID, existing.Status))
	case store.Done(existing.Status):
		return fail(fmt.Errorf("task %s is already %s; it cannot be run again", spec.ID, existing.Status))
	default:
		if existing.Status == store.TaskRunning {
			fmt.Fprintf(env.Stderr, "mill task run: %s was left running (mill stopped mid-run?); retrying\n", spec.ID)
		}
		if err := s.UpdateTaskSpec(ctx, spec); err != nil {
			return fail(err)
		}
	}

	return runTaskHere(ctx, env, paths, cfg, *hostFlag, s, spec.ID)
}

// runTaskHere runs the task in this process; the caller holds the run lock.
func runTaskHere(ctx context.Context, env Env, paths project.Paths, cfg config.Config, hostKind string, s *store.Store, id string) int {
	p, err := newPipeline(env, "task run", paths, cfg, hostKind, s, env.Stdout)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	out, err := p.Run(ctx, id)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	if out.Status != store.TaskVerified {
		return ExitNotVerified
	}
	printReviewHint(ctx, env, s, id)
	return ExitOK
}

func printReviewHint(ctx context.Context, env Env, s *store.Store, id string) {
	if br, err := s.GetBranch(ctx, id); err == nil {
		from := br.StartSHA
		if from == "" {
			from = br.BaseSHA
		}
		fmt.Fprintf(env.Stdout, "\nReview: git -C %s diff %s..%s\n", br.Worktree, short(from), br.Name)
	}
}

// joinScheduler queues the task for the scheduler (pid) that holds the run lock and waits for
// its result. It never repairs interrupted state while the scheduler is live; when the
// scheduler stops before finishing the task, task run takes the lock and runs the task itself.
func joinScheduler(ctx context.Context, env Env, paths project.Paths, cfg config.Config, hostKind string, s *store.Store, spec taskspec.Spec, retry bool, pid int) int {
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	existing, err := s.GetTask(ctx, spec.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if retry {
			return fail(fmt.Errorf("task %s does not exist yet; run it without --retry", spec.ID))
		}
		if err := s.CreateTask(ctx, spec); err != nil {
			return fail(err)
		}
	case err != nil:
		return fail(err)
	case !retry:
		return fail(fmt.Errorf("task %s already exists (status %s); use --retry to run it again", spec.ID, existing.Status))
	case store.Done(existing.Status):
		return fail(fmt.Errorf("task %s is already %s; it cannot be run again", spec.ID, existing.Status))
	case existing.Status == store.TaskRunning:
		doing, err := runningDoing(ctx, s, existing)
		if err != nil {
			return fail(err)
		}
		return fail(fmt.Errorf("task %s is %s under the running scheduler (pid %d); wait for it to finish (follow it with `mill watch`), then retry", spec.ID, doing, pid))
	default:
		if err := s.UpdateTaskSpec(ctx, spec); err != nil {
			return fail(err)
		}
		if existing.Status != store.TaskPending {
			if err := s.SetTaskStatus(ctx, spec.ID, store.TaskPending, "", scheduler.ReasonRetry); err != nil {
				return fail(err)
			}
		}
	}
	fmt.Fprintf(env.Stdout, "queued %s; the running scheduler (pid %d) will run it\n", spec.ID, pid)

	for {
		if ctx.Err() != nil {
			break
		}
		t, err := s.GetTask(ctx, spec.ID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fail(err)
		}
		if finalStatus(t.Status) {
			return printJoinedOutcome(ctx, env, s, t)
		}
		_, held, err := runlock.Held(paths.RunLock)
		if err != nil {
			return fail(err)
		}
		if !held {
			// The scheduler stopped without finishing the task: run it here.
			release, err := lockAndRecover(ctx, env, "task run", paths, s, cfg.Prices)
			if err != nil {
				if _, h, herr := runlock.Held(paths.RunLock); herr == nil && h {
					continue // another scheduler took the lock first; it runs the task
				}
				return fail(err)
			}
			defer release()
			t, err := s.GetTask(ctx, spec.ID)
			if err != nil {
				return fail(err)
			}
			if finalStatus(t.Status) {
				return printJoinedOutcome(ctx, env, s, t)
			}
			fmt.Fprintf(env.Stdout, "the scheduler stopped before running %s; running it here\n", spec.ID)
			return runTaskHere(ctx, env, paths, cfg, hostKind, s, spec.ID)
		}
		select {
		case <-ctx.Done():
		case <-time.After(env.taskRunPoll()):
		}
	}
	fmt.Fprintf(env.Stderr, "mill task run: stopped waiting; %s stays queued and the running scheduler (pid %d) will still run it (see `mill task show %s`)\n", spec.ID, pid, spec.ID)
	return ExitError
}

// finalStatus reports whether a task has ended: verified (or later), blocked or failed.
func finalStatus(status string) bool {
	return store.Done(status) || status == store.TaskBlocked || status == store.TaskFailed
}

// runningDoing names what a running task is doing: its running attempt, or its phase.
func runningDoing(ctx context.Context, s *store.Store, t store.Task) (string, error) {
	attempts, err := s.Attempts(ctx, t.ID)
	if err != nil {
		return "", err
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		if a := attempts[i]; a.Status == store.AttemptRunning {
			return fmt.Sprintf("running attempt %d (%s)", a.N, a.Role), nil
		}
	}
	if t.Phase != "" {
		return "running (" + t.Phase + ")", nil
	}
	return "running", nil
}

// printJoinedOutcome prints the result of a task another process ran, like the pipeline's
// outcome line, and returns task run's exit code for it.
func printJoinedOutcome(ctx context.Context, env Env, s *store.Store, t store.Task) int {
	u, err := s.TaskUsage(ctx, t.ID)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	total := fmt.Sprintf("%s tok, $%.2f", fmtInt(u.Tokens(budget.CountBillable)), u.CostUSD)
	if !store.Done(t.Status) {
		fmt.Fprintf(env.Stdout, "%s: %s: %s (task total %s)\n", t.ID, t.Status, t.Reason, total)
		return ExitNotVerified
	}
	head := ""
	if br, err := s.GetBranch(ctx, t.ID); err == nil {
		head = " at " + short(br.HeadSHA)
	}
	fmt.Fprintf(env.Stdout, "%s: %s%s (task total %s)\n", t.ID, t.Status, head, total)
	printReviewHint(ctx, env, s, t.ID)
	return ExitOK
}

// lockAndRecover takes the run lock and repairs what a killed run left behind. Call release
// when the run ends.
func lockAndRecover(ctx context.Context, env Env, name string, paths project.Paths, s *store.Store, prices map[string]price.Rates) (release func(), err error) {
	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		return nil, err
	}
	release = func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "mill %s: %v\n", name, err)
		}
	}
	if err := killLeftoverWorkers(ctx, env.Stdout, s, herdrTabs{runs: paths.Runs, lookPath: env.lookPath}); err != nil {
		release()
		return nil, err
	}
	usage, err := interruptedUsage(ctx, env.Stdout, s, prices)
	if err != nil {
		release()
		return nil, err
	}
	tasks, attempts, err := s.RecoverInterrupted(ctx, usage)
	if err != nil {
		release()
		return nil, err
	}
	if tasks > 0 || attempts > 0 {
		fmt.Fprintf(env.Stdout, "recovered %d interrupted task(s) and %d running attempt(s) from a previous run\n", tasks, attempts)
	}
	return release, nil
}

// interruptedUsage reads the raw stream of every running attempt that has one and returns the
// usage to record when the attempts are abandoned. Streams without a result event get an
// estimated cost. It prints what was recovered.
func interruptedUsage(ctx context.Context, out io.Writer, s *store.Store, prices map[string]price.Rates) (map[int64]store.AbandonedUsage, error) {
	running, err := s.RunningAttempts(ctx)
	if err != nil {
		return nil, err
	}
	usage := map[int64]store.AbandonedUsage{}
	var total budget.Usage
	for _, a := range running {
		if a.LogPath == "" {
			continue
		}
		if _, err := os.Stat(a.LogPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("recover usage of %s attempt %d: %w", a.TaskID, a.N, err)
		}
		u, model, finished, err := runner.UsageFromStream(a.LogPath)
		if err != nil {
			return nil, fmt.Errorf("recover usage of %s attempt %d: %w", a.TaskID, a.N, err)
		}
		if u == (budget.Usage{}) {
			continue
		}
		estimated := false
		if !finished {
			if usd, ok := price.Estimate(model, u, prices); ok {
				u.CostUSD, estimated = usd, true
			}
		}
		usage[a.ID] = store.AbandonedUsage{Usage: u, CostEstimated: estimated}
		total = total.Add(u)
	}
	if len(usage) > 0 {
		fmt.Fprintf(out, "recorded %s tokens (billable, ~$%.2f) from %d interrupted attempt(s)\n",
			fmtInt(total.Tokens(budget.CountBillable)), total.CostUSD, len(usage))
	}
	return usage, nil
}

// newPipeline builds the pipeline from the project config: host, runner and agent env.
// hostKind overrides host.kind from the config when not empty.
func newPipeline(env Env, name string, paths project.Paths, cfg config.Config, hostKind string, s *store.Store, out io.Writer) (*pipeline.Pipeline, error) {
	if hostKind == "" {
		hostKind = cfg.Host.Kind
	}
	h, _, err := host.Select(hostKind, env.getenv, env.lookPath)
	if err != nil {
		return nil, err
	}
	var r runner.Runner
	var agentEnv []string
	switch cfg.Runner.Kind {
	case "claude":
		cl := runner.NewClaude(cfg.Runner.ClaudeBin, h)
		cl.Prices = cfg.Prices
		r = cl
		agentEnv = runner.ClaudeEnvAllow
	case "fake":
		sp := cfg.Runner.FakeScript
		if !filepath.IsAbs(sp) {
			sp = filepath.Join(paths.Root, sp)
		}
		script, err := runner.LoadFakeScript(sp)
		if err != nil {
			return nil, err
		}
		r = runner.NewFake(script)
	}
	home := env.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	return &pipeline.Pipeline{
		Store: s, Runner: r, Host: h, Cfg: cfg, Paths: paths, Home: home,
		Out: out, AgentEnvAllow: agentEnv,
	}, nil
}

type taskView struct {
	Schema   string           `json:"schema"`
	Task     store.Task       `json:"task"`
	Branch   *store.Branch    `json:"branch,omitempty"`
	Attempts []store.Attempt  `json:"attempts"`
	Usage    budget.Usage     `json:"usage"`
	Findings []prompt.Finding `json:"findings,omitempty"` // last failed attempt's findings
	Verdict  json.RawMessage  `json:"verdict,omitempty"`  // last verifier verdict
	RunsDir  string           `json:"runs_dir"`
	// NextModel is the model the task's next implementer attempt would run on, "" when the
	// config maps none; NextModelWhy says where it comes from or what to add.
	NextModel    string `json:"next_model"`
	NextModelWhy string `json:"next_model_why"`
	// DependsOn is a goal task's dependencies (full ids); DepsAdded lists the dependencies
	// added after the plan was approved, oldest first.
	DependsOn []string    `json:"depends_on,omitempty"`
	DepsAdded []depsAdded `json:"deps_added,omitempty"`
	// SkippedTests are the test skips the task's latest skipped-tests event recorded.
	SkippedTests []skiptest.Skip `json:"skipped_tests,omitempty"`
}

// latestSkippedTests returns the test skips recorded by the last skipped-tests event of a
// task (either the current kind or the older skipped_tests), nil when there is none. evs
// are the task's events in seq order.
func latestSkippedTests(evs []store.Event) ([]skiptest.Skip, error) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != pipeline.SkippedTestsEvent && evs[i].Kind != pipeline.OldSkippedTestsEvent {
			continue
		}
		var d struct {
			Skips []skiptest.Skip `json:"skips"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil {
			return nil, fmt.Errorf("corrupt %s event %d: %w", evs[i].Kind, evs[i].Seq, err)
		}
		return d.Skips, nil
	}
	return nil, nil
}

// printSkippedTests prints the skips a task added, one "file:line  reason" line each, and
// nothing when there are none.
func printSkippedTests(w io.Writer, skips []skiptest.Skip) {
	if len(skips) == 0 {
		return
	}
	fmt.Fprintf(w, "\nSkipped tests added (%d):\n", len(skips))
	for _, s := range skips {
		line := fmt.Sprintf("  %s:%d", s.File, s.Line)
		if s.Reason != "" {
			line += "  " + s.Reason
		}
		fmt.Fprintln(w, line)
	}
}

// depsAdded is one task-deps-added event: when the dependencies were added and their full ids.
type depsAdded struct {
	At    time.Time `json:"at"`
	Added []string  `json:"added"`
}

func runTaskShow(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "task show")
	asJSON := fs.Bool("json", false, "print one JSON document")
	// Allow the id before or after flags.
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
	} else if fs.NArg() > 0 || id == "" {
		fmt.Fprintln(env.Stderr, "Usage: mill task show <id> [--json]")
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task show: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	v := taskView{Schema: "mill/task@1", RunsDir: filepath.Join(paths.Runs, id)}
	if v.Task, err = s.GetTask(ctx, id); err != nil {
		return fail(err)
	}
	if br, err := s.GetBranch(ctx, id); err == nil {
		v.Branch = &br
	}
	if v.Attempts, err = s.Attempts(ctx, id); err != nil {
		return fail(err)
	}
	if v.Usage, err = s.TaskUsage(ctx, id); err != nil {
		return fail(err)
	}
	if !store.Done(v.Task.Status) {
		if v.Findings, err = pipeline.LastFindings(ctx, s, id); err != nil {
			return fail(err)
		}
	}
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return fail(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "task-verdict" {
			var d struct {
				Verdict json.RawMessage `json:"verdict"`
			}
			if json.Unmarshal(evs[i].Data, &d) == nil {
				v.Verdict = d.Verdict
			}
			break
		}
	}
	if v.NextModel, v.NextModelWhy, err = nextImplementerModel(cfg, v.Task, v.Attempts, evs); err != nil {
		return fail(err)
	}
	// An override is the only reason that does not name its source on its own.
	if v.NextModelWhy == "override" {
		v.NextModelWhy = "task override"
	}
	goalID, _ := store.SplitTaskID(id)
	if goalID != "" {
		if v.DependsOn, err = s.TaskDeps(ctx, id); err != nil {
			return fail(err)
		}
	}
	if v.SkippedTests, err = latestSkippedTests(evs); err != nil {
		return fail(fmt.Errorf("task %s: %w", id, err))
	}
	for _, e := range evs {
		if e.Kind != "task-deps-added" {
			continue
		}
		var d struct {
			Added []string `json:"added"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fail(fmt.Errorf("task %s: bad task-deps-added event %d: %w", id, e.Seq, err))
		}
		added := make([]string, 0, len(d.Added))
		for _, l := range d.Added {
			added = append(added, store.TaskID(goalID, l))
		}
		v.DepsAdded = append(v.DepsAdded, depsAdded{At: e.At, Added: added})
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	printTask(env, v)
	return ExitOK
}

func runTaskList(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "task list")
	asJSON := fs.Bool("json", false, "print one JSON document")
	status := fs.String("status", "", "only tasks with this status (running includes claimed tasks)")
	all := fs.Bool("all", false, "list every task, oldest first")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *status != "" && !slices.Contains(store.TaskStatuses, *status) {
		fmt.Fprintf(env.Stderr, "mill task list: unknown status %q (valid: %s)\n", *status, strings.Join(store.TaskStatuses, ", "))
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill task list: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	opts := taskListOptions{Status: *status, All: *all, JSON: *asJSON}
	if err := renderTaskList(ctx, env.Stdout, s, opts, time.Now()); err != nil {
		return fail(err)
	}
	return ExitOK
}

// taskListRecent is how many tasks that are neither current nor blocked/failed the default
// task list shows.
const taskListRecent = 20

type taskListOptions struct {
	Status string // only tasks with this status; "running" includes claimed tasks
	All    bool   // every task oldest first, instead of current work first
	JSON   bool
}

// listedTask is a task list row: the task and the live claim on it, if any.
type listedTask struct {
	store.TaskSummary
	Claim *store.TaskClaim `json:"claim,omitempty"`
}

// renderTaskList writes the task list to w. By default it shows running and claimed tasks,
// then blocked and failed ones, then the taskListRecent newest others; opts.All and
// opts.JSON list every task oldest first. now is the clock for claim held times.
func renderTaskList(ctx context.Context, w io.Writer, s *store.Store, opts taskListOptions, now time.Time) error {
	summaries, err := s.TaskSummaries(ctx, "")
	if err != nil {
		return err
	}
	claims, err := s.ClaimedTasks(ctx)
	if err != nil {
		return err
	}
	tasks := []listedTask{}
	for _, t := range summaries {
		lt := listedTask{TaskSummary: t}
		if c, ok := claims[t.ID]; ok {
			lt.Claim = &c
		}
		switch {
		case opts.Status == "":
		case opts.Status == store.TaskRunning && lt.Claim != nil:
		case t.Status != opts.Status:
			continue
		}
		tasks = append(tasks, lt)
	}

	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		doc := struct {
			Schema string       `json:"schema"`
			Tasks  []listedTask `json:"tasks"`
		}{"mill/task-list@1", tasks}
		return enc.Encode(doc)
	}
	if len(tasks) == 0 {
		_, err := fmt.Fprintln(w, "no tasks")
		return err
	}
	rows, hidden := tasks, 0
	if !opts.All {
		rows, hidden = currentFirst(tasks)
	}
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tATTEMPTS\tTOKENS\tCOST\tTITLE")
	for _, t := range rows {
		st := t.Status
		if t.Phase != "" {
			st += " (" + t.Phase + ")"
		}
		// A running task claimed by mill run is already shown as running; the claim adds nothing.
		if c := t.Claim; c != nil && !(c.Command == "run" && t.Status == store.TaskRunning) {
			doing := c.Activity
			if doing == "" {
				doing = "mill " + c.Command
			}
			st += fmt.Sprintf(" (claimed: %s, held %s)", doing, fmtElapsed(now.Sub(c.ClaimedAt)))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%.2f\t%s\n", t.ID, st, t.Attempts,
			fmtInt(t.Usage.Tokens(budget.CountBillable)), t.Usage.CostUSD, t.Title)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if hidden > 0 {
		_, err := fmt.Fprintf(w, "… %d older task(s) not shown; use --all\n", hidden)
		return err
	}
	return nil
}

// currentFirst orders tasks (given oldest first) for the default list: running and claimed
// tasks, then blocked and failed ones, both oldest first, then the others newest first,
// keeping only the taskListRecent newest of those. It returns the rows and how many it left out.
func currentFirst(tasks []listedTask) ([]listedTask, int) {
	var current, stuck, rest []listedTask
	for _, t := range tasks {
		switch {
		case t.Status == store.TaskRunning || t.Claim != nil:
			current = append(current, t)
		case t.Status == store.TaskBlocked || t.Status == store.TaskFailed:
			stuck = append(stuck, t)
		default:
			rest = append(rest, t)
		}
	}
	slices.Reverse(rest)
	hidden := 0
	if len(rest) > taskListRecent {
		hidden = len(rest) - taskListRecent
		rest = rest[:taskListRecent]
	}
	return append(append(current, stuck...), rest...), hidden
}

// nextImplementerModel is the model, and why, that t's next implementer attempt would run on:
// the config's base model for t (Config.TaskModel), escalated one tier (Config.Escalate) for
// every earlier implementer or fixer attempt with findings recorded, as the pipeline picks it.
func nextImplementerModel(cfg config.Config, t store.Task, attempts []store.Attempt, evs []store.Event) (model, why string, err error) {
	base, baseWhy := cfg.TaskModel(t.Size, t.Complexity, t.Model)
	if base == "" {
		return "", baseWhy, nil
	}
	withFindings := map[int]bool{}
	for _, ev := range evs {
		if ev.Kind != "attempt-findings" {
			continue
		}
		var d struct {
			Attempt int `json:"attempt"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return "", "", fmt.Errorf("read the findings event %d of %s: %w", ev.Seq, t.ID, err)
		}
		withFindings[d.Attempt] = true
	}
	failed := map[int]bool{}
	last := 0
	for _, a := range attempts {
		if (a.Role == store.RoleImplementer || a.Role == store.RoleFixer) && withFindings[a.N] {
			failed[a.N] = true
			last = max(last, a.N)
		}
	}
	model = cfg.Escalate(base, len(failed))
	if model != base {
		return model, fmt.Sprintf("escalated after attempt %d failed", last), nil
	}
	return base, baseWhy, nil
}

func printTask(env Env, v taskView) {
	w := env.Stdout
	t := v.Task
	fmt.Fprintf(w, "%s: %s\n", t.ID, t.Title)
	status := t.Status
	if t.Phase != "" {
		status += " (" + t.Phase + ")"
	}
	fmt.Fprintf(w, "  status    %s\n", status)
	if t.Reason != "" {
		fmt.Fprintf(w, "  reason    %s\n", t.Reason)
	}
	fmt.Fprintf(w, "  kind      %s, size %s\n", t.Kind, t.Size)
	complexity := t.Complexity
	if complexity == "" {
		complexity = "medium"
	}
	if t.ComplexityReason != "" {
		complexity += " (" + t.ComplexityReason + ")"
	}
	fmt.Fprintf(w, "  complexity  %s\n", complexity)
	model := v.NextModel
	if model == "" {
		model = "(none)"
	}
	fmt.Fprintf(w, "  model     %s (%s)\n", model, v.NextModelWhy)
	if len(v.DependsOn) > 0 {
		fmt.Fprintf(w, "  depends   %s\n", localIDs(v.DependsOn))
	}
	for _, da := range v.DepsAdded {
		fmt.Fprintf(w, "  dependencies added %s: %s\n", pipeline.FormatResetTime(da.At, time.Now()), localIDs(da.Added))
	}
	if b := v.Branch; b != nil {
		fmt.Fprintf(w, "  branch    %s (base %s @%s, head %s)\n", b.Name, b.Base, short(b.BaseSHA), short(b.HeadSHA))
		fmt.Fprintf(w, "  worktree  %s\n", b.Worktree)
	}
	estimated := false
	for _, a := range v.Attempts {
		estimated = estimated || a.CostEstimated
	}
	total := ""
	if estimated {
		total = " (includes estimated costs, marked ~)"
	}
	fmt.Fprintf(w, "  spend     %s tokens (billable), $%.2f%s\n", fmtInt(v.Usage.Tokens(budget.CountBillable)), v.Usage.CostUSD, total)
	fmt.Fprintf(w, "  runs      %s\n", v.RunsDir)

	if len(v.Attempts) > 0 {
		fmt.Fprintln(w, "\nAttempts:")
		tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  #\trole\tmodel\treason\tstatus\tin\tout\tcache w/r\tcost\ttime")
		for _, a := range v.Attempts {
			// The agent-reported duration when known, else the wall time.
			dur := "-"
			if a.EndedAt != nil || a.AgentDuration > 0 {
				dur = a.Duration().Round(time.Second).String()
			}
			mark := ""
			if a.CostEstimated {
				mark = "~"
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\t%s/%s\t%s$%.2f\t%s\n", a.N, a.Role, dash(a.Model), dash(a.ModelReason), a.Status,
				fmtInt(a.Usage.InputTokens), fmtInt(a.Usage.OutputTokens),
				fmtInt(a.Usage.CacheWriteTokens), fmtInt(a.Usage.CacheReadTokens), mark, a.Usage.CostUSD, dur)
		}
		tw.Flush()
	}
	if len(v.Verdict) > 0 {
		var vd prompt.VerifierOutput
		if json.Unmarshal(v.Verdict, &vd) == nil {
			fmt.Fprintf(w, "\nLast verdict: %s: %s\n", vd.Verdict, vd.Summary)
		}
	}
	printSkippedTests(w, v.SkippedTests)
	if len(v.Findings) > 0 {
		fmt.Fprintln(w, "\nLast findings:")
		for _, f := range v.Findings {
			fmt.Fprintf(w, "  - %s\n", strings.ReplaceAll(f.String(), "\n", "\n    "))
		}
	}
}

// dash shows "-" for an empty value, such as a model recorded before models were stored.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// localIDs joins the local part of each full task id with ", ".
func localIDs(ids []string) string {
	local := make([]string, 0, len(ids))
	for _, id := range ids {
		_, l := store.SplitTaskID(id)
		local = append(local, l)
	}
	return strings.Join(local, ", ")
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func fmtInt(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// killLeftoverWorkers kills the process groups of workers that outlived a killed mill and
// closes the herdr tabs they ran in, plus every herdr tab under the runs directory whose
// recorded mill process is gone. A recorded process is only killed when it is alive,
// leads its own group and its command line contains the recorded command; anything else
// (gone, or pid reused) is left alone.
func killLeftoverWorkers(ctx context.Context, out io.Writer, s *store.Store, tabs herdrTabs) error {
	running, err := s.RunningAttempts(ctx)
	if err != nil {
		return err
	}
	for _, a := range running {
		if err := killLeftoverProcesses(ctx, out, s, a); err != nil {
			return err
		}
		if err := closeRecordedTabs(out, tabs, a); err != nil {
			return err
		}
	}
	return closeOrphanedTabs(out, tabs)
}

// killLeftoverProcesses kills what is left of one running attempt's worker.
func killLeftoverProcesses(ctx context.Context, out io.Writer, s *store.Store, a store.Attempt) error {
	if a.PID <= 0 {
		return nil
	}
	pgid, command, alive, err := host.ProcessInfo(a.PID)
	if err != nil {
		return fmt.Errorf("check leftover worker %d (%s attempt %d %s): %w; stop it by hand and retry", a.PID, a.TaskID, a.N, a.Role, err)
	}
	if !alive {
		// The recorded leader is gone, but the agent may have left children running in
		// its group (e.g. a test run started from its Bash tool). The group id is not
		// proof they are ours: if the whole group died and recovery runs much later, the
		// pid can be reused by an unrelated process that starts its own group and exits,
		// leaving members. What identifies the worker's children is the working
		// directory: the agent and what it starts run inside the task's worktree, so
		// only members whose cwd is inside it are killed, one by one.
		members, err := host.GroupMembers(a.PID)
		if err != nil {
			return fmt.Errorf("check leftover processes of worker %d (%s attempt %d %s): %w; stop them by hand and retry", a.PID, a.TaskID, a.N, a.Role, err)
		}
		if len(members) == 0 {
			return nil
		}
		worktree, err := taskWorktree(ctx, s, a.TaskID)
		if err != nil {
			return fmt.Errorf("check leftover processes of worker %d (%s attempt %d %s): %w; stop them by hand and retry", a.PID, a.TaskID, a.N, a.Role, err)
		}
		killed, left := 0, 0
		for _, m := range members {
			if worktree == "" || !insideDir(m, worktree) {
				left++
				continue
			}
			if err := host.KillProcess(m); err != nil {
				return fmt.Errorf("kill leftover process %d of worker %d (%s attempt %d %s): %w; stop it by hand and retry", m, a.PID, a.TaskID, a.N, a.Role, err)
			}
			killed++
		}
		if killed > 0 {
			fmt.Fprintf(out, "killed %d leftover process(es) of worker %d (%s attempt %d %s), whose agent had already exited\n", killed, a.PID, a.TaskID, a.N, a.Role)
		}
		if left > 0 {
			fmt.Fprintf(out, "left %d process(es) of group %d alone: not running inside the task's worktree\n", left, a.PID)
		}
		return nil
	}
	if pgid != a.PID || a.Command == "" || !strings.Contains(command, a.Command) {
		return nil
	}
	if err := host.KillGroup(pgid); err != nil {
		return fmt.Errorf("kill leftover worker %d (%s attempt %d %s): %w; stop it by hand and retry", a.PID, a.TaskID, a.N, a.Role, err)
	}
	fmt.Fprintf(out, "killed leftover worker %d (%s attempt %d %s)\n", a.PID, a.TaskID, a.N, a.Role)
	return nil
}

// taskWorktree returns the task's recorded worktree with symlinks resolved, or "" when the
// task has no recorded (or no longer existing) worktree.
func taskWorktree(ctx context.Context, s *store.Store, taskID string) (string, error) {
	b, err := s.GetBranch(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if b.Worktree == "" {
		return "", nil
	}
	wt, err := filepath.EvalSymlinks(b.Worktree)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve worktree %s: %w", b.Worktree, err)
	}
	return wt, nil
}

// insideDir reports whether pid's working directory is dir or below it. An unreadable
// working directory is not inside.
func insideDir(pid int, dir string) bool {
	cwd, err := host.ProcessCwd(pid)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	rel, err := filepath.Rel(dir, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// hostFlagVar registers --host, which overrides host.kind for one command.
func hostFlagVar(fs *flag.FlagSet) *string {
	return fs.String("host", "", "host for agent processes: auto, exec or herdr (default: host.kind from the config)")
}

func validHostFlag(env Env, name, v string) bool {
	switch v {
	case "", "auto", "exec", "herdr":
		return true
	}
	fmt.Fprintf(env.Stderr, "mill %s: --host must be auto, exec or herdr, got %q\n", name, v)
	return false
}
