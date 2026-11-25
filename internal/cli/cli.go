// Package cli implements the mill command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/doctor"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// Version is set at build time: -ldflags "-X github.com/msidorkevich/mill/internal/cli.Version=v0.1.0".
var Version = "dev"

// Exit codes are stable (CL2).
const (
	ExitOK          = 0 // success
	ExitError       = 1 // the command failed
	ExitUsage       = 2 // bad command line
	ExitCheckFailed = 3 // a check reported failure (doctor)
)

// Env is what a command needs from the outside world. Tests pass their own.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Dir    string // working directory
	Home   string // user home; "" = os.UserHomeDir()
	// Doctor overrides doctor options (tool lookup) in tests; nil uses the real machine.
	Doctor *doctor.Options
	// Getenv reads the process environment (EDITOR, HERDR_*); nil = os.Getenv.
	Getenv func(string) string
	// LookPath finds programs on PATH (herdr); nil = exec.LookPath.
	LookPath func(string) (string, error)
	// TaskRunPoll is how often task run reads the store while a running scheduler works on
	// its task; 0 = 1s.
	TaskRunPoll time.Duration
	// SchedulerPoll is how often mill run reads the store for new work; 0 =
	// scheduler.DefaultPollInterval.
	SchedulerPoll time.Duration
	// KeepWait blocks for d or until ctx is done and reports whether the wait completed
	// (keep --watch between passes); nil = a real timer.
	KeepWait func(ctx context.Context, d time.Duration) bool
}

// getenv returns the value of the environment variable key through env.Getenv.
func (env Env) getenv(key string) string {
	if env.Getenv == nil {
		return os.Getenv(key)
	}
	return env.Getenv(key)
}

// lookPath finds file on PATH through env.LookPath.
func (env Env) lookPath(file string) (string, error) {
	if env.LookPath == nil {
		return exec.LookPath(file)
	}
	return env.LookPath(file)
}

// taskRunPoll is env.TaskRunPoll or its default.
func (env Env) taskRunPoll() time.Duration {
	if env.TaskRunPoll <= 0 {
		return time.Second
	}
	return env.TaskRunPoll
}

// keepWait waits through env.KeepWait or, when nil, a real timer.
func (env Env) keepWait(ctx context.Context, d time.Duration) bool {
	if env.KeepWait != nil {
		return env.KeepWait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env Env, args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"init", "set up .mill/ in the current git repository", runInit},
		{"doctor", "check the machine and repository (--json)", runDoctor},
		{"task", "run or show a single task (run, show)", runTask},
		{"plan", "create, review and approve a plan (show, edit, approve)", runPlan},
		{"goal", "list or show goals (list, show)", runGoal},
		{"run", "run every approved goal and queued task as the repository's scheduler, or one goal (--retry-blocked)", runRun},
		{"review", "list tasks awaiting review; show, changes, reject, approve (--goal)", runReview},
		{"sync", "detect merged reviews, mark them landed, restack and re-publish children (--json)", runSync},
		{"keep", "keep branches mergeable: sync, rebase, fix, re-publish, combined check (--watch, --interval, --combined, --json)", runKeep},
		{"clean", "remove worktrees and branches of landed tasks (--remote, --dry-run)", runClean},
		{"watch", "show running agents, live spend and their latest action (--once, --interval)", runWatch},
		{"stats", "report first-pass rate, attempts and cost by size, complexity and model (models, --json)", runStats},
		{"tui", "interactive terminal UI", runTUI},
		{"version", "print the mill version", runVersion},
		{"help", "show this help", func(_ context.Context, env Env, _ []string) int {
			usage(env.Stdout)
			return ExitOK
		}},
	}
}

// Run executes the command line args (without the program name) and returns an exit code.
func Run(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		usage(env.Stderr)
		return ExitUsage
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, env, args[1:])
		}
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(env.Stdout)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "mill: unknown command %q\n\n", args[0])
	usage(env.Stderr)
	return ExitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: mill <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Exit codes: 0 ok, 1 error, 2 usage error, 3 check failed, 4 task or goal not verified.")
}

func newFlags(env Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("mill "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

var flagErrHelp = flag.ErrHelp

// parse handles -h (exit 0) and bad flags (exit 2). ok=false means return code.
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected arguments: %s\n", fs.Name(), strings.Join(fs.Args(), " "))
		return ExitUsage, false
	}
	return 0, true
}

func runVersion(_ context.Context, env Env, args []string) int {
	fs := newFlags(env, "version")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fmt.Fprintf(env.Stdout, "mill %s\n", Version)
	return ExitOK
}

func runInit(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "init")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	root, err := project.GitRoot(env.Dir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}
	p := project.PathsAt(root)
	if err := os.MkdirAll(p.Runs, 0o755); err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}

	branch, branchErr := currentBranch(ctx, root)
	detected := branchErr == nil
	if !detected {
		branch = "main"
	}
	cfgState, err := writeIfMissing(p.Config, config.RenderDefaultTOML(branch))
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}
	cfg, err := config.Load(p.Config)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: existing %s is invalid: %v\n", p.Config, err)
		return ExitError
	}
	ignState, err := syncGitignore(p.GitIgnore)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}

	s, err := store.Open(ctx, p.DB, store.Options{})
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}
	defer s.Close()
	if _, err := s.AppendEvent(ctx, "init", "", "", map[string]string{"version": Version}); err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}
	v, _ := s.SchemaVersion(ctx)

	fmt.Fprintf(env.Stdout, "Initialized mill in %s\n", p.MillDir)
	fmt.Fprintf(env.Stdout, "  config.toml  %s\n", cfgState)
	fmt.Fprintf(env.Stdout, "  base branch  %s\n", cfg.BaseBranch)
	if !detected && cfgState == "created" {
		fmt.Fprintf(env.Stdout, "  could not read the current branch (%v); wrote base_branch = \"main\". Check base_branch in .mill/config.toml.\n", branchErr)
	}
	fmt.Fprintf(env.Stdout, "  .gitignore   %s\n", ignState)
	fmt.Fprintf(env.Stdout, "  mill.db      schema v%d\n", v)
	fmt.Fprintf(env.Stdout, "  runs/        ready\n")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Next: set check_cmd in .mill/config.toml, commit .mill/config.toml and .mill/.gitignore, then run `mill doctor`.")
	return ExitOK
}

// currentBranch returns the branch HEAD points to; it works before the first commit.
// It fails on a detached HEAD.
func currentBranch(ctx context.Context, root string) (string, error) {
	out, err := exec.CommandContext(ctx, gitx.Bin(), "-C", root, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("HEAD is detached or unreadable: %w", err)
	}
	b := strings.TrimSpace(string(out))
	if b == "" {
		return "", errors.New("git reported an empty branch name")
	}
	return b, nil
}

// writeIfMissing creates path with text unless it exists. It never overwrites.
func writeIfMissing(path, text string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return "kept (already exists)", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return "", err
	}
	return "created", f.Close()
}

// syncGitignore creates path with the required lines, or appends the ones an existing file
// lacks. Existing lines and their order are kept.
func syncGitignore(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return writeIfMissing(path, doctor.GitignoreText())
	}
	if err != nil {
		return "", err
	}
	missing := doctor.MissingGitignoreLines(string(b))
	if len(missing) == 0 {
		return "kept (already exists)", nil
	}
	add := strings.Join(missing, "\n") + "\n"
	if len(b) > 0 && b[len(b)-1] != '\n' {
		add = "\n" + add
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(add); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return "updated (added: " + strings.Join(missing, ", ") + ")", nil
}

func runDoctor(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "doctor")
	asJSON := fs.Bool("json", false, "print the report as one JSON document")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	var opts doctor.Options
	if env.Doctor != nil {
		opts = *env.Doctor
	}
	opts.Dir = env.Dir
	r := doctor.Run(ctx, opts)

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema string `json:"schema"`
			doctor.Report
		}{"mill/doctor@1", r}); err != nil {
			fmt.Fprintf(env.Stderr, "mill doctor: %v\n", err)
			return ExitError
		}
	} else {
		marks := map[doctor.Status]string{doctor.OK: "ok  ", doctor.Warn: "warn", doctor.Fail: "FAIL", doctor.Skip: "skip"}
		for _, c := range r.Checks {
			fmt.Fprintf(env.Stdout, "%s  %-9s %s\n", marks[c.Status], c.Name, c.Detail)
		}
	}
	if !r.OK {
		return ExitCheckFailed
	}
	return ExitOK
}
