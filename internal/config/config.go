// Package config loads and validates a project's .mill/config.toml.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/price"
)

// Config is the project configuration. Every field has a default (see Default).
type Config struct {
	// CheckCmd is the project gate, run with `sh -c` in a task's worktree. Empty means
	// every gate fails (P3: a gate with no checks is not a pass).
	CheckCmd    string `toml:"check_cmd"`
	BaseBranch  string `toml:"base_branch"`
	Remote      string `toml:"remote"`
	MaxParallel int    `toml:"max_parallel"`
	// MaxParallelGates bounds the gates running at the same time across every mill
	// process on the repository.
	MaxParallelGates int `toml:"max_parallel_gates"`
	// MaxInFlight caps the tasks started and not yet finished, whether they run an agent or
	// sit in a gate; unset means MaxParallel + MaxParallelGates. It must be >= MaxParallel.
	MaxInFlight int `toml:"max_in_flight"`
	MaxAttempts int `toml:"max_attempts"`
	// OwnsPolicy says what happens to changes outside a task's owns: "warn", "enforce" or "off".
	OwnsPolicy      string `toml:"owns_policy"`
	StartChildrenOn string `toml:"start_children_on"`
	// CleanOnLand says what sync removes when a task lands: "all" (worktree, local branch
	// and remote review branch), "local" (worktree and local branch) or "off".
	CleanOnLand string `toml:"clean_on_land"`
	// WorktreeRoot holds task worktrees, outside the repository so that workers never
	// write inside it. "" means ~/.cache/mill/worktrees.
	WorktreeRoot string `toml:"worktree_root"`
	// GateTimeout bounds each gate command.
	GateTimeout Duration `toml:"gate_timeout"`
	// GateRerun re-runs the failed gate commands once; if they all pass then, the gate
	// passes and the verifier is told which commands were flaky.
	GateRerun bool `toml:"gate_rerun"`
	// GateCheckBase runs the commands that still fail on the task's starting point, so a
	// check that is already broken there blocks the task instead of becoming a finding.
	GateCheckBase bool `toml:"gate_check_base"`
	// GateNice runs gate commands under `nice -n GateNice` so they yield the CPU to
	// interactive work. 0 disables it.
	GateNice int `toml:"gate_nice"`
	// ImplementerRunsCheck tells implementer, fixer and resolver agents to run check_cmd
	// themselves before they finish. When false, mill runs check_cmd only in the gate and
	// the agents run the task's checks and the tests of the packages they changed.
	ImplementerRunsCheck bool `toml:"implementer_runs_check"`
	// EnvAllow lists extra environment variables passed to workers and gate commands, on top
	// of the built-in allowlist (SE3).
	EnvAllow []string `toml:"env_allow"`
	// OnPublish is a shell command run with `sh -c` when a task's branch is published. Empty
	// means no hook.
	OnPublish string `toml:"on_publish"`
	// OnUpdate is a shell command run with `sh -c` when a task's review is updated. Empty
	// means no hook.
	OnUpdate string `toml:"on_update"`
	// HookEnv lists extra environment variables passed to hook commands, on top of the
	// built-in allowlist. Each name must be a valid environment variable name.
	HookEnv []string `toml:"hook_env"`
	// HookTimeout bounds each hook command.
	HookTimeout Duration     `toml:"hook_timeout"`
	Runner      RunnerConfig `toml:"runner"`
	Host        HostConfig   `toml:"host"`
	// Sizes is the legacy size -> model mapping, used only when the file has no [models].
	Sizes map[string]Size `toml:"sizes"`
	// Models maps a task's size and complexity to a model: size -> complexity -> model.
	// Nil means the mapping comes from Sizes (legacy fallback).
	Models map[string]map[string]string `toml:"models"`
	// ModelTiers orders model families from cheapest to strongest. A model belongs to the
	// first tier whose name it contains; escalation moves up this list.
	ModelTiers []string    `toml:"model_tiers"`
	Roles      RolesConfig `toml:"roles"`
	Budget     Budget      `toml:"budget"`
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
	APITimeout     Duration `toml:"api_timeout"`     // bounds one API request of a claude run (API_TIMEOUT_MS)
}

// HostConfig selects where worker processes run.
type HostConfig struct {
	Kind string `toml:"kind"` // "auto", "exec" or "herdr"
}

// Size maps a task size to a model (PL6).
type Size struct {
	Model string `toml:"model"`
}

// RolesConfig holds the models of the non-implementing roles.
type RolesConfig struct {
	// Planner is the model that plans goals.
	Planner string `toml:"planner"`
	// VerifierMin is the lowest model a verifier runs on; a verifier never runs below the
	// implementer's tier.
	VerifierMin string `toml:"verifier_min"`
}

// Budget holds the spending limits (BG1, BG2, BG6).
type Budget struct {
	Count budget.Count `toml:"count"`
	// VerifierShare is the share of a task size's attempt limits reserved before the verifier
	// runs (0 < share <= 1).
	VerifierShare float64               `toml:"verifier_share"`
	Attempt       budget.Limits         `toml:"attempt"`
	Size          map[string]SizeBudget `toml:"size"`
	Task          budget.Limits         `toml:"task"`
	Goal          budget.Limits         `toml:"goal"`
}

// SizeBudget overrides the attempt limits for one task size.
type SizeBudget struct {
	Attempt budget.Limits `toml:"attempt"`
}

// Known values.
var (
	RunnerKinds      = []string{"claude", "fake"}
	HostKinds        = []string{"auto", "exec", "herdr"}
	OwnsPolicies     = []string{"warn", "enforce", "off"}
	ChildrenOnModes  = []string{"verified", "published"}
	CleanOnLandModes = []string{"all", "local", "off"}
	SizeNames        = []string{"S", "M", "L"}
	Complexities     = []string{"low", "medium", "high"}
)

// Default returns the built-in defaults. DefaultTOML documents the same values.
func Default() Config {
	return Config{
		CheckCmd:             "",
		BaseBranch:           "main",
		Remote:               "origin",
		MaxParallel:          2,
		MaxParallelGates:     1,
		MaxInFlight:          3,
		MaxAttempts:          3,
		OwnsPolicy:           "warn",
		StartChildrenOn:      "verified",
		CleanOnLand:          "all",
		WorktreeRoot:         "",
		GateTimeout:          Duration{20 * time.Minute},
		GateRerun:            true,
		GateCheckBase:        true,
		GateNice:             DefaultGateNice(),
		ImplementerRunsCheck: false,
		EnvAllow:             []string{},
		HookTimeout:          Duration{60 * time.Second},
		Runner: RunnerConfig{
			Kind:           "claude",
			ClaudeBin:      "claude",
			AttemptTimeout: Duration{30 * time.Minute},
			APITimeout:     Duration{15 * time.Minute},
		},
		Host: HostConfig{Kind: "auto"},
		Models: map[string]map[string]string{
			"S": {"low": "haiku", "medium": "sonnet", "high": "opus"},
			"M": {"low": "sonnet", "medium": "opus", "high": "opus"},
		},
		ModelTiers: []string{"haiku", "sonnet", "opus"},
		Roles:      RolesConfig{Planner: "opus", VerifierMin: "sonnet"},
		Budget: Budget{
			Count:         budget.CountBillable,
			VerifierShare: 0.25,
			Attempt:       budget.Limits{USD: 2, Tokens: 400_000},
			Size: map[string]SizeBudget{
				"S": {Attempt: budget.Limits{USD: 1, Tokens: 200_000}},
				"M": {Attempt: budget.Limits{USD: 3, Tokens: 600_000}},
			},
			Task: budget.Limits{USD: 8},
			Goal: budget.Limits{Tokens: 6_000_000},
		},
	}
}

// DefaultGateNice is the default gate_nice: 10 on darwin and linux, where nice is standard,
// 0 elsewhere.
func DefaultGateNice() int {
	switch runtime.GOOS {
	case "darwin", "linux":
		return 10
	}
	return 0
}

// DefaultTOML is the config file `mill init` writes. Decoding it yields Default().
const DefaultTOML = `# mill project configuration. Commit this file; .mill/runs/ and the database are ignored.

# The gate: run with "sh -c" in each task's worktree. Required: an empty gate fails every task.
check_cmd = ""

base_branch = "main"
remote = "origin"
max_parallel = 2          # tasks running an agent at the same time
max_parallel_gates = 1    # gates running at the same time, across all mill processes
# max_in_flight = 3       # tasks started and not finished (agents and gates); default max_parallel + max_parallel_gates
max_attempts = 3          # implement attempts per task before it is blocked
start_children_on = "verified"   # or "published"
owns_policy = "warn"      # changes outside a task's owns: "warn" (verifier must justify them), "enforce" (finding, no gate) or "off"

clean_on_land = "all"     # when sync finds a task landed, remove: "all" (worktree, local branch, remote review branch), "local" (worktree and local branch) or "off"

worktree_root = ""        # where task worktrees live; "" = ~/.cache/mill/worktrees
gate_timeout = "20m"      # per gate command
gate_rerun = true         # re-run failed gate commands once; passing then = flaky, the gate passes
gate_check_base = true    # run still-failing commands on the task's starting point; failing there too blocks the task
# gate_nice = 10          # run gate commands under "nice -n <value>"; 0 disables. Default: 10 on macOS and Linux, 0 elsewhere
implementer_runs_check = false   # true: implementer, fixer and resolver agents run check_cmd themselves; false: only the gate runs it
env_allow = []            # extra env vars for workers and gates, e.g. ["GOPROXY", "NPM_TOKEN"]
# on_publish = ""         # shell command run with "sh -c" when a task's branch is published; empty = no hook
# on_update = ""          # shell command run with "sh -c" when a task's review is updated; empty = no hook
# hook_env = []           # extra env vars for hook commands, e.g. ["SLACK_WEBHOOK"]
# hook_timeout = "60s"    # per hook command

# Model families from cheapest to strongest. A model belongs to the first tier whose name it
# contains ("claude-opus-5-5" is opus); retries and verifiers move up this list.
model_tiers = ["haiku", "sonnet", "opus"]

[runner]
kind = "claude"           # or "fake" (scripted, zero tokens; set fake_script)
claude_bin = "claude"
attempt_timeout = "30m"   # per agent run
api_timeout = "15m"       # per API request of a claude run, passed as API_TIMEOUT_MS

[host]
kind = "auto"             # "auto": herdr inside a herdr pane (HERDR_ENV=1, herdr on PATH), else exec; "exec": plain processes; "herdr": always herdr, fails outside a herdr pane

# Model per task size and complexity. A file with [models] uses only its entries; a file
# without [models] but with legacy [sizes.X] model = "..." blocks maps every complexity of a
# size to that model.
[models]
S = { low = "haiku", medium = "sonnet", high = "opus" }
M = { low = "sonnet", medium = "opus", high = "opus" }
# L = { low = "opus", medium = "opus", high = "opus" }

[roles]
planner = "opus"          # model that plans goals
verifier_min = "sonnet"   # verifiers run on the implementer's model or this, whichever tier is higher

[budget]
count = "billable"        # tokens counted against limits: "billable" or "all" (adds cache reads)
verifier_share = 0.25   # share of a task size's attempt limits reserved before the verifier runs (0 < share <= 1)

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
	cfg.Models = nil
	cfg.ModelTiers = nil
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
	// [models] wins; without it, [sizes] is the legacy mapping (Models stays nil); with
	// neither, the default mapping applies.
	if !md.IsDefined("models") && !md.IsDefined("sizes") {
		cfg.Models = def.Models
	}
	if !md.IsDefined("model_tiers") {
		cfg.ModelTiers = def.ModelTiers
	}
	if !md.IsDefined("budget", "size") {
		cfg.Budget.Size = def.Budget.Size
	}
	if !md.IsDefined("max_in_flight") {
		cfg.MaxInFlight = cfg.MaxParallel + cfg.MaxParallelGates
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
	if c.MaxParallelGates < 1 {
		add("max_parallel_gates must be >= 1, got %d", c.MaxParallelGates)
	}
	if c.MaxInFlight < c.MaxParallel {
		add("max_in_flight must be >= max_parallel (%d), got %d; raise it or remove it to use max_parallel + max_parallel_gates", c.MaxParallel, c.MaxInFlight)
	}
	if c.MaxAttempts < 1 {
		add("max_attempts must be >= 1, got %d", c.MaxAttempts)
	}
	if !oneOf(c.StartChildrenOn, ChildrenOnModes) {
		add("start_children_on must be one of %v, got %q", ChildrenOnModes, c.StartChildrenOn)
	}
	if !oneOf(c.CleanOnLand, CleanOnLandModes) {
		add("clean_on_land must be one of %v, got %q", CleanOnLandModes, c.CleanOnLand)
	}
	if !oneOf(c.OwnsPolicy, OwnsPolicies) {
		add("owns_policy must be one of %v, got %q", OwnsPolicies, c.OwnsPolicy)
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
	if c.Runner.APITimeout.Duration <= 0 {
		add("runner.api_timeout must be > 0")
	}
	if c.GateTimeout.Duration <= 0 {
		add("gate_timeout must be > 0")
	}
	if c.GateNice < 0 {
		add("gate_nice must be >= 0 (0 disables nice), got %d", c.GateNice)
	}
	for _, k := range c.EnvAllow {
		if !envKeyRe.MatchString(k) {
			add("env_allow: %q is not a valid environment variable name", k)
		}
	}
	if c.HookTimeout.Duration <= 0 {
		add("hook_timeout must be > 0, got %s", c.HookTimeout)
	}
	for _, k := range c.HookEnv {
		if !envKeyRe.MatchString(k) {
			add("hook_env: %q is not a valid environment variable name", k)
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
	for size, byComplexity := range c.Models {
		if !oneOf(size, SizeNames) {
			add("models.%s: unknown size, want one of %v", size, SizeNames)
		}
		for cx, m := range byComplexity {
			if !oneOf(cx, Complexities) {
				add("models.%s.%s: unknown complexity, want one of %v", size, cx, Complexities)
			}
			if m == "" {
				add("models.%s.%s must not be empty", size, cx)
			}
		}
	}
	if len(c.ModelTiers) == 0 {
		add("model_tiers must not be empty")
	}
	seen := map[string]bool{}
	for _, t := range c.ModelTiers {
		if t == "" {
			add("model_tiers: tier names must not be empty")
		} else if seen[t] {
			add("model_tiers: %q appears more than once", t)
		}
		seen[t] = true
	}
	if c.Roles.Planner == "" {
		add("roles.planner must not be empty")
	}
	if c.Roles.VerifierMin == "" {
		add("roles.verifier_min must not be empty")
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
	if s := c.Budget.VerifierShare; !(s > 0 && s <= 1) {
		add("budget.verifier_share must be > 0 and <= 1, got %v", s)
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

// VerifierBudget returns the limits reserved for the verifier of a task of the given size: each
// non-zero unit of AttemptBudget(size) times Budget.VerifierShare. Tokens round up; a zero
// (unlimited) unit stays zero.
func (c Config) VerifierBudget(size string) budget.Limits {
	l := c.AttemptBudget(size)
	if l.USD != 0 {
		l.USD *= c.Budget.VerifierShare
	}
	if l.Tokens != 0 {
		l.Tokens = int64(math.Ceil(float64(l.Tokens) * c.Budget.VerifierShare))
	}
	return l
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

// Model returns the model for a task size at medium complexity, or "" if there is no mapping.
func (c Config) Model(size string) string {
	m, _ := c.TaskModel(size, "medium", "")
	return m
}

// TaskModel picks the model for a task: the override when one is given, else the mapping for
// size and complexity. why says where the model came from; when there is no mapping, model
// is "" and why names the config key to add.
func (c Config) TaskModel(size, complexity, override string) (model, why string) {
	if override != "" {
		return override, "override"
	}
	if c.Models == nil {
		if m := c.Sizes[size].Model; m != "" {
			return m, fmt.Sprintf("mapping %s/%s (sizes fallback)", size, complexity)
		}
		return "", fmt.Sprintf("no model for size %q: add [sizes.%s] model = \"...\" or a [models] section with %s.%s", size, size, size, complexity)
	}
	if m := c.Models[size][complexity]; m != "" {
		return m, fmt.Sprintf("mapping %s/%s", size, complexity)
	}
	return "", fmt.Sprintf("no model for size %q, complexity %q: add %s = \"...\" to [models.%s]", size, complexity, complexity, size)
}

// Tier returns the index in ModelTiers of the first tier name contained in model, or -1 when
// the model belongs to no tier.
func (c Config) Tier(model string) int {
	for i, t := range c.ModelTiers {
		if t != "" && strings.Contains(model, t) {
			return i
		}
	}
	return -1
}

// Escalate returns the model steps tiers above model, capped at the top tier. A model that
// belongs to no tier, or that would not move, is returned unchanged.
func (c Config) Escalate(model string, steps int) string {
	t := c.Tier(model)
	if t < 0 || steps <= 0 {
		return model
	}
	n := min(t+steps, len(c.ModelTiers)-1)
	if n == t {
		return model
	}
	return c.ModelTiers[n]
}

// VerifierModel returns the model a verifier runs on for an implementer on impl: the higher of
// impl and Roles.VerifierMin by tier. An untiered impl is returned unchanged.
func (c Config) VerifierModel(impl string) string {
	it := c.Tier(impl)
	if it < 0 {
		return impl
	}
	if c.Tier(c.Roles.VerifierMin) > it {
		return c.Roles.VerifierMin
	}
	return impl
}

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

// RenderDefaultTOML returns DefaultTOML with base_branch set to branch, keeping its comments.
func RenderDefaultTOML(branch string) string {
	return strings.Replace(DefaultTOML, `base_branch = "main"`, "base_branch = "+strconv.Quote(branch), 1)
}
