// Package config loads and validates a project's .mill/config.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"mill/internal/budget"
	"mill/internal/price"
)

// Config is the project configuration. Every field has a default (see Default).
type Config struct {
	// CheckCmd is the project gate, run with `sh -c` in a task's worktree. Empty means
	// every gate fails (P3: a gate with no checks is not a pass).
	CheckCmd        string `toml:"check_cmd"`
	BaseBranch      string `toml:"base_branch"`
	Remote          string `toml:"remote"`
	MaxParallel     int    `toml:"max_parallel"`
	MaxAttempts     int    `toml:"max_attempts"`
	StartChildrenOn string `toml:"start_children_on"`
	// WorktreeRoot holds task worktrees, outside the repository so that workers never
	// write inside it. "" means ~/.cache/mill/worktrees.
	WorktreeRoot string `toml:"worktree_root"`
	// GateTimeout bounds each gate command.
	GateTimeout Duration `toml:"gate_timeout"`
	// EnvAllow lists extra environment variables passed to workers and gate commands, on top
	// of the built-in allowlist (SE3).
	EnvAllow []string        `toml:"env_allow"`
	Runner   RunnerConfig    `toml:"runner"`
	Host     HostConfig      `toml:"host"`
	Sizes    map[string]Size `toml:"sizes"`
	Budget   Budget          `toml:"budget"`
	// Prices overrides the built-in token prices (USD per million tokens), keyed by model
	// alias or id. Used to estimate the cost of runs that end without a result.
	Prices map[string]price.Rates `toml:"prices"`
}

// Duration is a time.Duration written as a string such as "20m" in TOML.
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// RunnerConfig selects the agent runner.
type RunnerConfig struct {
	Kind           string   `toml:"kind"`            // "claude" or "fake"
	FakeScript     string   `toml:"fake_script"`     // path to a fake-runner script, relative to the repo root
	ClaudeBin      string   `toml:"claude_bin"`      // claude executable, looked up on PATH
	AttemptTimeout Duration `toml:"attempt_timeout"` // bounds one agent run
}

// HostConfig selects where worker processes run.
type HostConfig struct {
	Kind string `toml:"kind"` // "auto", "exec" or "herdr"
}

// Size maps a task size to a model (PL6).
type Size struct {
	Model string `toml:"model"`
}

// Budget holds the spending limits (BG1, BG2, BG6).
type Budget struct {
	Count   budget.Count          `toml:"count"`
	Attempt budget.Limits         `toml:"attempt"`
	Size    map[string]SizeBudget `toml:"size"`
	Task    budget.Limits         `toml:"task"`
	Goal    budget.Limits         `toml:"goal"`
}

// SizeBudget overrides the attempt limits for one task size.
type SizeBudget struct {
	Attempt budget.Limits `toml:"attempt"`
}

// Known values.
var (
	RunnerKinds     = []string{"claude", "fake"}
	HostKinds       = []string{"auto", "exec", "herdr"}
	ChildrenOnModes = []string{"verified", "published"}
	SizeNames       = []string{"S", "M", "L"}
)

// Default returns the built-in defaults. DefaultTOML documents the same values.
func Default() Config {
	return Config{
		CheckCmd:        "",
		BaseBranch:      "main",
		Remote:          "origin",
		MaxParallel:     2,
		MaxAttempts:     3,
		StartChildrenOn: "verified",
		WorktreeRoot:    "",
		GateTimeout:     Duration{20 * time.Minute},
		EnvAllow:        []string{},
		Runner: RunnerConfig{
			Kind:           "claude",
			ClaudeBin:      "claude",
			AttemptTimeout: Duration{30 * time.Minute},
		},
		Host: HostConfig{Kind: "auto"},
		Sizes: map[string]Size{
			"S": {Model: "sonnet"},
			"M": {Model: "opus"},
		},
		Budget: Budget{
			Count:   budget.CountBillable,
			Attempt: budget.Limits{USD: 2, Tokens: 400_000},
			Size: map[string]SizeBudget{
				"S": {Attempt: budget.Limits{USD: 1, Tokens: 200_000}},
				"M": {Attempt: budget.Limits{USD: 3, Tokens: 600_000}},
			},
			Task: budget.Limits{USD: 8},
			Goal: budget.Limits{Tokens: 6_000_000},
		},
	}
}

// DefaultTOML is the config file `mill init` writes. Decoding it yields Default().
const DefaultTOML = `# mill project configuration. Commit this file; .mill/runs/ and the database are ignored.

# The gate: run with "sh -c" in each task's worktree. Required: an empty gate fails every task.
check_cmd = ""

base_branch = "main"
remote = "origin"
max_parallel = 2          # tasks running at the same time
max_attempts = 3          # implement attempts per task before it is blocked
start_children_on = "verified"   # or "published"

worktree_root = ""        # where task worktrees live; "" = ~/.cache/mill/worktrees
gate_timeout = "20m"      # per gate command
env_allow = []            # extra env vars for workers and gates, e.g. ["GOPROXY", "NPM_TOKEN"]

[runner]
kind = "claude"           # or "fake" (scripted, zero tokens; set fake_script)
claude_bin = "claude"
attempt_timeout = "30m"   # per agent run

[host]
kind = "auto"             # "auto" (herdr if available, else exec), "exec" or "herdr"

[sizes.S]
model = "sonnet"
[sizes.M]
model = "opus"

[budget]
count = "billable"        # tokens counted against limits: "billable" or "all" (adds cache reads)

[budget.attempt]          # per attempt; 0 or absent = no limit in that unit
usd = 2.0
tokens = 400_000

[budget.size.S]
attempt = { usd = 1.0, tokens = 200_000 }
[budget.size.M]
attempt = { usd = 3.0, tokens = 600_000 }

[budget.task]
usd = 8.0

[budget.goal]
tokens = 6_000_000

# Optional price overrides, USD per million tokens, used to estimate the cost of runs that end
# without a result from claude. Keyed by model alias or id; built-in rates cover the Claude 5
# models.
# [prices.opus]
# input = 4.0
# output = 20.0
# cache_write_5m = 5.0
# cache_write_1h = 8.0
# cache_read = 0.20
`

// Load reads path on top of the defaults and validates the result. Unknown keys are errors,
// so a typo never silently falls back to a default.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(string(data))
}

// Parse decodes TOML text on top of the defaults and validates the result.
func Parse(text string) (Config, error) {
	cfg := Default()
	// Maps are replaced, not merged, when the file sets them.
	cfg.Sizes = nil
	cfg.Budget.Size = nil
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	def := Default()
	if !md.IsDefined("sizes") {
		cfg.Sizes = def.Sizes
	}
	if !md.IsDefined("budget", "size") {
		cfg.Budget.Size = def.Budget.Size
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks every field and reports all problems at once.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.BaseBranch == "" {
		add("base_branch must not be empty")
	}
	if c.Remote == "" {
		add("remote must not be empty")
	}
	if c.MaxParallel < 1 {
		add("max_parallel must be >= 1, got %d", c.MaxParallel)
	}
	if c.MaxAttempts < 1 {
		add("max_attempts must be >= 1, got %d", c.MaxAttempts)
	}
	if !oneOf(c.StartChildrenOn, ChildrenOnModes) {
		add("start_children_on must be one of %v, got %q", ChildrenOnModes, c.StartChildrenOn)
	}
	if !oneOf(c.Runner.Kind, RunnerKinds) {
		add("runner.kind must be one of %v, got %q", RunnerKinds, c.Runner.Kind)
	}
	if c.Runner.Kind == "fake" && c.Runner.FakeScript == "" {
		add("runner.fake_script is required when runner.kind = \"fake\"")
	}
	if c.Runner.Kind == "claude" && c.Runner.ClaudeBin == "" {
		add("runner.claude_bin must not be empty")
	}
	if c.Runner.AttemptTimeout.Duration <= 0 {
		add("runner.attempt_timeout must be > 0")
	}
	if c.GateTimeout.Duration <= 0 {
		add("gate_timeout must be > 0")
	}
	for _, k := range c.EnvAllow {
		if !envKeyRe.MatchString(k) {
			add("env_allow: %q is not a valid environment variable name", k)
		}
	}
	if !oneOf(c.Host.Kind, HostKinds) {
		add("host.kind must be one of %v, got %q", HostKinds, c.Host.Kind)
	}
	for name, s := range c.Sizes {
		if !oneOf(name, SizeNames) {
			add("sizes.%s: unknown size, want one of %v", name, SizeNames)
		}
		if s.Model == "" {
			add("sizes.%s.model must not be empty", name)
		}
	}
	for name, r := range c.Prices {
		for key, v := range map[string]float64{"input": r.Input, "output": r.Output,
			"cache_write_5m": r.CacheWrite5m, "cache_write_1h": r.CacheWrite1h, "cache_read": r.CacheRead} {
			if v < 0 {
				add("prices.%s.%s must be >= 0, got %v", name, key, v)
			}
		}
	}
	if !c.Budget.Count.Valid() {
		add("budget.count must be %q or %q, got %q", budget.CountBillable, budget.CountAll, c.Budget.Count)
	}
	check := func(name string, l budget.Limits) {
		if err := l.Validate(); err != nil {
			add("budget.%s: %v", name, err)
		}
	}
	check("attempt", c.Budget.Attempt)
	check("task", c.Budget.Task)
	check("goal", c.Budget.Goal)
	for name, sb := range c.Budget.Size {
		if !oneOf(name, SizeNames) {
			add("budget.size.%s: unknown size, want one of %v", name, SizeNames)
		}
		check("size."+name+".attempt", sb.Attempt)
	}
	return errors.Join(errs...)
}

// AttemptBudget returns the per-attempt limits for a task of the given size: the default
// attempt limits with that size's non-zero values on top.
func (c Config) AttemptBudget(size string) budget.Limits {
	return c.Budget.Attempt.Override(c.Budget.Size[size].Attempt)
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Worktrees returns the directory holding task worktrees for the repository at root:
// <worktree_root>/<repo name>-<short hash of root>. home is the user's home directory.
func (c Config) Worktrees(root, home string) string {
	base := c.WorktreeRoot
	if base == "" {
		base = filepath.Join(home, ".cache", "mill", "worktrees")
	} else if strings.HasPrefix(base, "~/") {
		base = filepath.Join(home, base[2:])
	}
	return filepath.Join(base, repoKey(root))
}

// Model returns the model for a task size, or "" if the size has no mapping.
func (c Config) Model(size string) string { return c.Sizes[size].Model }

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// repoKey names a repository's worktree directory: its base name plus a short FNV hash of
// the full path, so two repositories with the same name never share a directory.
func repoKey(root string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(root); i++ {
		h ^= uint32(root[i])
		h *= 16777619
	}
	return fmt.Sprintf("%s-%08x", filepath.Base(root), h)
}
