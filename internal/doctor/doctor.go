// Package doctor checks that the machine and the repository are ready for mill.
// It spends no tokens: agent CLIs are only located and asked for their version.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// Status of one check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is the result of one check.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the result of all checks.
type Report struct {
	Checks []Check `json:"checks"`
	OK     bool    `json:"ok"` // false if any check failed
}

// MinGit is the oldest git mill supports (merge-tree --write-tree, GT1).
var MinGit = [3]int{2, 38, 0}

// Options configure Run. Zero values use the real environment.
type Options struct {
	Dir      string                                            // directory to check from
	GitBin   func() string                                     // git binary mill runs; defaults to gitx.Bin
	LookPath func(string) (string, error)                      // defaults to exec.LookPath
	Output   func(name string, args ...string) (string, error) // runs a command, returns stdout
	Getenv   func(string) string                               // defaults to os.Getenv
}

func (o *Options) defaults() {
	if o.GitBin == nil {
		o.GitBin = gitx.Bin
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Output == nil {
		o.Output = func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()
			return string(out), err
		}
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
}

// Run performs every check. Later checks are skipped when the ones they depend on fail.
func Run(ctx context.Context, opts Options) Report {
	opts.defaults()
	var r Report
	add := func(name string, st Status, format string, a ...any) {
		r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: fmt.Sprintf(format, a...)})
	}

	// git
	gitOK := false
	gitBin := opts.GitBin()
	if _, err := opts.LookPath(gitBin); err != nil {
		add("git", Fail, "git not found at %s: install git or put it on PATH", gitBin)
	} else if out, err := opts.Output(gitBin, "--version"); err != nil {
		add("git", Fail, "git --version failed at %s: %v", gitBin, err)
	} else if v, err := ParseGitVersion(out); err != nil {
		add("git", Fail, "%v (from %s)", err, gitBin)
	} else if !atLeast(v, MinGit) {
		add("git", Fail, "git %s at %s is too old; need >= %s", fmtVer(v), gitBin, fmtVer(MinGit))
	} else {
		add("git", OK, "git %s (>= %s) at %s", fmtVer(v), fmtVer(MinGit), gitBin)
		gitOK = true
	}

	// repository and .mill/
	var paths project.Paths
	repoOK, initOK := false, false
	if !gitOK {
		add("repo", Skip, "needs git")
	} else if root, err := project.GitRoot(opts.Dir); err != nil {
		add("repo", Fail, "%v", err)
	} else {
		add("repo", OK, "%s", root)
		repoOK = true
		paths = project.PathsAt(root)
	}
	if !repoOK {
		add("init", Skip, "needs a git repository")
	} else if _, err := os.Stat(paths.Config); err != nil {
		add("init", Fail, "%v", project.ErrNotInitialized)
	} else {
		add("init", OK, "%s", paths.MillDir)
		initOK = true
	}

	// config
	var cfg config.Config
	cfgOK := false
	if !initOK {
		add("config", Skip, "needs mill init")
	} else if c, err := config.Load(paths.Config); err != nil {
		add("config", Fail, "%v", err)
	} else {
		cfg, cfgOK = c, true
		if strings.TrimSpace(c.CheckCmd) == "" {
			add("config", Warn, "check_cmd is empty: every gate will fail until it is set")
		} else {
			add("config", OK, "check_cmd = %q", c.CheckCmd)
		}
	}

	// database
	if !initOK {
		add("database", Skip, "needs mill init")
	} else if _, err := os.Stat(paths.DB); err != nil {
		add("database", Fail, "%s missing (run `mill init`)", paths.DB)
	} else if s, err := store.Open(ctx, paths.DB, store.Options{}); err != nil {
		add("database", Fail, "%v", err)
	} else {
		v, verr := s.SchemaVersion(ctx)
		mode, merr := s.JournalMode(ctx)
		s.Close()
		switch {
		case verr != nil || merr != nil:
			add("database", Fail, "read database state: %v", errors.Join(verr, merr))
		case mode != "wal":
			add("database", Fail, "journal mode is %q, want wal", mode)
		default:
			add("database", OK, "schema v%d, %s mode", v, mode)
		}
	}

	// runner
	if !cfgOK {
		add("runner", Skip, "needs a valid config")
	} else {
		switch cfg.Runner.Kind {
		case "claude":
			if _, err := opts.LookPath("claude"); err != nil {
				add("runner", Fail, "runner.kind = claude but `claude` is not on PATH")
			} else if out, err := opts.Output("claude", "--version"); err != nil {
				add("runner", Fail, "claude --version failed: %v", err)
			} else {
				add("runner", OK, "claude %s (login not checked: that would spend tokens)", strings.TrimSpace(out))
			}
		case "fake":
			p := cfg.Runner.FakeScript
			if !filepath.IsAbs(p) {
				p = filepath.Join(paths.Root, p)
			}
			if _, err := os.Stat(p); err != nil {
				add("runner", Fail, "fake script %s: %v", p, err)
			} else {
				add("runner", OK, "fake runner, script %s", p)
			}
		}
	}

	// host
	if !cfgOK {
		add("host", Skip, "needs a valid config")
	} else if _, desc, err := host.Select(cfg.Host.Kind, opts.Getenv, opts.LookPath); err != nil {
		add("host", Fail, "%v", err)
	} else {
		add("host", OK, "%s", desc)
	}

	// gitignore
	if !initOK {
		add("gitignore", Skip, "needs mill init")
	} else if b, err := os.ReadFile(paths.GitIgnore); err != nil {
		add("gitignore", Warn, "read %s: %v; run `mill init` to create it", paths.GitIgnore, err)
	} else if missing := MissingGitignoreLines(string(b)); len(missing) > 0 {
		add("gitignore", Warn, "%s lacks %s; run `mill init` to add them", paths.GitIgnore, strings.Join(missing, ", "))
	} else {
		add("gitignore", OK, "%s has every required line", paths.GitIgnore)
	}

	r.OK = true
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.OK = false
		}
	}
	return r
}

// GitignoreLines are the lines .mill/.gitignore must contain to keep machine-local state
// out of git. `mill init` and the gitignore check both use this one list.
var GitignoreLines = []string{"runs/", "mill.db", "mill.db-*", "run.lock"}

// GitignoreText is the content `mill init` writes to a new .mill/.gitignore.
func GitignoreText() string {
	return "# machine-local mill state\n" + strings.Join(GitignoreLines, "\n") + "\n"
}

// MissingGitignoreLines returns the required lines absent from text, in required order.
func MissingGitignoreLines(text string) []string {
	have := map[string]bool{}
	for _, l := range strings.Split(text, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var missing []string
	for _, l := range GitignoreLines {
		if !have[l] {
			missing = append(missing, l)
		}
	}
	return missing
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)(?:\.(\d+))?`)

// ParseGitVersion extracts major.minor.patch from `git --version` output.
func ParseGitVersion(out string) ([3]int, error) {
	m := gitVersionRe.FindStringSubmatch(out)
	if m == nil {
		return [3]int{}, fmt.Errorf("cannot parse git version from %q", strings.TrimSpace(out))
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		if m[i+1] != "" {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return v, nil
}

func atLeast(v, min [3]int) bool {
	for i := 0; i < 3; i++ {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

func fmtVer(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
