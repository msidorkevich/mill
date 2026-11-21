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
	"strings"

	"mill/internal/config"
	"mill/internal/doctor"
	"mill/internal/project"
	"mill/internal/store"
)

// Version is set at build time: -ldflags "-X mill/internal/cli.Version=v0.1.0".
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
	fmt.Fprintln(w, "Exit codes: 0 ok, 1 error, 2 usage error, 3 check failed, 4 task not verified.")
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

// gitignoreText keeps machine-local state out of git; config.toml stays committed.
const gitignoreText = "# machine-local mill state\nruns/\nmill.db\nmill.db-*\n"

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

	cfgState, err := writeIfMissing(p.Config, config.DefaultTOML)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill init: %v\n", err)
		return ExitError
	}
	if _, err := config.Load(p.Config); err != nil {
		fmt.Fprintf(env.Stderr, "mill init: existing %s is invalid: %v\n", p.Config, err)
		return ExitError
	}
	ignState, err := writeIfMissing(p.GitIgnore, gitignoreText)
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
	fmt.Fprintf(env.Stdout, "  .gitignore   %s\n", ignState)
	fmt.Fprintf(env.Stdout, "  mill.db      schema v%d\n", v)
	fmt.Fprintf(env.Stdout, "  runs/        ready\n")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Next: set check_cmd in .mill/config.toml, commit .mill/config.toml and .mill/.gitignore, then run `mill doctor`.")
	return ExitOK
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
