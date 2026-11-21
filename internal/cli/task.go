package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"mill/internal/budget"
	"mill/internal/config"
	"mill/internal/host"
	"mill/internal/pipeline"
	"mill/internal/project"
	"mill/internal/prompt"
	"mill/internal/runlock"
	"mill/internal/runner"
	"mill/internal/store"
	"mill/internal/taskspec"
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
		fmt.Fprintln(w, "  mill task list [--status S] [--json]       list all tasks, oldest first")
		return code
	}
	switch args[0] {
	case "run":
		return runTaskRun(ctx, env, args[1:])
	case "show":
		return runTaskShow(ctx, env, args[1:])
	case "list":
		return runTaskList(ctx, env, args[1:])
	}
	fmt.Fprintf(env.Stderr, "mill task: unknown subcommand %q\n", args[0])
	return ExitUsage
}

// openProject finds the repository, loads its config and opens its store.
func openProject(ctx context.Context, env Env) (project.Paths, config.Config, *store.Store, error) {
	paths, err := project.Find(env.Dir)
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
	if code, ok := parse(fs, args); !ok {
		return code
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

	release, err := lockAndRecover(ctx, env, "task run", paths, s)
	if err != nil {
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
	case existing.Status == store.TaskVerified:
		return fail(fmt.Errorf("task %s is already verified", spec.ID))
	default:
		if existing.Status == store.TaskRunning {
			fmt.Fprintf(env.Stderr, "mill task run: %s was left running (mill stopped mid-run?); retrying\n", spec.ID)
		}
		if err := s.UpdateTaskSpec(ctx, spec); err != nil {
			return fail(err)
		}
	}

	p, err := newPipeline(env, "task run", paths, cfg, s, env.Stdout)
	if err != nil {
		return fail(err)
	}
	out, err := p.Run(ctx, spec.ID)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill task run: %v\n", err)
		return ExitError
	}
	if out.Status != store.TaskVerified {
		return ExitNotVerified
	}
	if br, err := s.GetBranch(ctx, spec.ID); err == nil {
		from := br.StartSHA
		if from == "" {
			from = br.BaseSHA
		}
		fmt.Fprintf(env.Stdout, "\nReview: git -C %s diff %s..%s\n", br.Worktree, short(from), br.Name)
	}
	return ExitOK
}

// lockAndRecover takes the run lock and repairs what a killed run left behind. Call release
// when the run ends.
func lockAndRecover(ctx context.Context, env Env, name string, paths project.Paths, s *store.Store) (release func(), err error) {
	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		return nil, err
	}
	release = func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "mill %s: %v\n", name, err)
		}
	}
	tasks, attempts, err := s.RecoverInterrupted(ctx)
	if err != nil {
		release()
		return nil, err
	}
	if tasks > 0 || attempts > 0 {
		fmt.Fprintf(env.Stdout, "recovered %d interrupted task(s) and %d running attempt(s) from a previous run\n", tasks, attempts)
	}
	return release, nil
}

// newPipeline builds the pipeline from the project config: host, runner and agent env.
func newPipeline(env Env, name string, paths project.Paths, cfg config.Config, s *store.Store, out io.Writer) (*pipeline.Pipeline, error) {
	h := host.Host(host.NewExec())
	if cfg.Host.Kind == "herdr" {
		fmt.Fprintf(env.Stderr, "mill %s: the herdr host is not implemented yet; using exec\n", name)
	}
	var r runner.Runner
	var agentEnv []string
	switch cfg.Runner.Kind {
	case "claude":
		r = runner.NewClaude(cfg.Runner.ClaudeBin, h)
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
	paths, _, s, err := openProject(ctx, env)
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
	if v.Task.Status != store.TaskVerified {
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
	status := fs.String("status", "", "only tasks with this status")
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
	tasks, err := s.TaskSummaries(ctx, *status)
	if err != nil {
		return fail(err)
	}
	if tasks == nil {
		tasks = []store.TaskSummary{}
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		doc := struct {
			Schema string              `json:"schema"`
			Tasks  []store.TaskSummary `json:"tasks"`
		}{"mill/task-list@1", tasks}
		if err := enc.Encode(doc); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	if len(tasks) == 0 {
		fmt.Fprintln(env.Stdout, "no tasks")
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tATTEMPTS\tTOKENS\tCOST\tTITLE")
	for _, t := range tasks {
		st := t.Status
		if t.Phase != "" {
			st += " (" + t.Phase + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%.2f\t%s\n", t.ID, st, t.Attempts,
			fmtInt(t.Usage.Tokens(budget.CountBillable)), t.Usage.CostUSD, t.Title)
	}
	tw.Flush()
	return ExitOK
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
	if b := v.Branch; b != nil {
		fmt.Fprintf(w, "  branch    %s (base %s @%s, head %s)\n", b.Name, b.Base, short(b.BaseSHA), short(b.HeadSHA))
		fmt.Fprintf(w, "  worktree  %s\n", b.Worktree)
	}
	fmt.Fprintf(w, "  spend     %s tokens (billable), $%.2f\n", fmtInt(v.Usage.Tokens(budget.CountBillable)), v.Usage.CostUSD)
	fmt.Fprintf(w, "  runs      %s\n", v.RunsDir)

	if len(v.Attempts) > 0 {
		fmt.Fprintln(w, "\nAttempts:")
		tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  #\trole\tstatus\tin\tout\tcache w/r\tcost\ttime")
		for _, a := range v.Attempts {
			dur := "-"
			if a.EndedAt != nil {
				dur = a.EndedAt.Sub(a.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s/%s\t$%.2f\t%s\n", a.N, a.Role, a.Status,
				fmtInt(a.Usage.InputTokens), fmtInt(a.Usage.OutputTokens),
				fmtInt(a.Usage.CacheWriteTokens), fmtInt(a.Usage.CacheReadTokens), a.Usage.CostUSD, dur)
		}
		tw.Flush()
	}
	if len(v.Verdict) > 0 {
		var vd prompt.VerifierOutput
		if json.Unmarshal(v.Verdict, &vd) == nil {
			fmt.Fprintf(w, "\nLast verdict: %s: %s\n", vd.Verdict, vd.Summary)
		}
	}
	if len(v.Findings) > 0 {
		fmt.Fprintln(w, "\nLast findings:")
		for _, f := range v.Findings {
			fmt.Fprintf(w, "  - %s\n", strings.ReplaceAll(f.String(), "\n", "\n    "))
		}
	}
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
