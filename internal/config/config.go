// Package config loads and validates a project's .mill/config.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"mill/internal/budget"
)

// Config is the project configuration. Every field has a default (see Default).
type Config struct {
	// CheckCmd is the project gate, run with `sh -c` in a task's worktree. Empty means
	// every gate fails (P3: a gate with no checks is not a pass).
	CheckCmd        string          `toml:"check_cmd"`
	BaseBranch      string          `toml:"base_branch"`
	Remote          string          `toml:"remote"`
	MaxParallel     int             `toml:"max_parallel"`
	MaxAttempts     int             `toml:"max_attempts"`
	StartChildrenOn string          `toml:"start_children_on"`
	Runner          RunnerConfig    `toml:"runner"`
	Host            HostConfig      `toml:"host"`
	Sizes           map[string]Size `toml:"sizes"`
	Budget          Budget          `toml:"budget"`
}

// RunnerConfig selects the agent runner.
type RunnerConfig struct {
	Kind       string `toml:"kind"`        // "claude" or "fake"
	FakeScript string `toml:"fake_script"` // path to a fake-runner script, relative to the repo root
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
		Runner:          RunnerConfig{Kind: "claude"},
		Host:            HostConfig{Kind: "auto"},
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

[runner]
kind = "claude"           # or "fake" (scripted, zero tokens; set fake_script)

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
