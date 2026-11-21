package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mill/internal/budget"
)

func TestDefaultTOMLMatchesDefault(t *testing.T) {
	cfg, err := Parse(DefaultTOML)
	if err != nil {
		t.Fatalf("DefaultTOML does not parse: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("DefaultTOML and Default() differ:\n toml: %+v\n code: %+v", cfg, Default())
	}
}

func TestEmptyFileGivesDefaults(t *testing.T) {
	cfg, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("empty config = %+v, want defaults", cfg)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := Parse(`
check_cmd = "go test ./..."
max_parallel = 4
[budget]
count = "all"
[budget.attempt]
tokens = 100
[budget.size.S]
attempt = { usd = 0.5 }
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CheckCmd != "go test ./..." || cfg.MaxParallel != 4 {
		t.Fatalf("top-level overrides not applied: %+v", cfg)
	}
	if cfg.Budget.Count != budget.CountAll {
		t.Fatalf("count = %q", cfg.Budget.Count)
	}
	// Untouched fields keep their defaults.
	if cfg.MaxAttempts != 3 || cfg.Budget.Attempt.USD != 2 {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	// A file that sets budget.size replaces the default size map.
	if _, ok := cfg.Budget.Size["M"]; ok {
		t.Fatal("budget.size.M kept from defaults although the file set budget.size")
	}
	// S: usd from the size, tokens from the attempt default.
	if got := cfg.AttemptBudget("S"); got != (budget.Limits{USD: 0.5, Tokens: 100}) {
		t.Fatalf("AttemptBudget(S) = %+v", got)
	}
	// A size with no entry uses the attempt defaults.
	if got := cfg.AttemptBudget("M"); got != (budget.Limits{USD: 2, Tokens: 100}) {
		t.Fatalf("AttemptBudget(M) = %+v", got)
	}
}

func TestAttemptBudgetDefaults(t *testing.T) {
	cfg := Default()
	if got := cfg.AttemptBudget("S"); got != (budget.Limits{USD: 1, Tokens: 200_000}) {
		t.Fatalf("S = %+v", got)
	}
	if got := cfg.AttemptBudget("L"); got != (budget.Limits{USD: 2, Tokens: 400_000}) {
		t.Fatalf("L (no size entry) = %+v", got)
	}
	if cfg.Model("M") != "opus" || cfg.Model("L") != "" {
		t.Fatalf("Model mapping wrong")
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	_, err := Parse("check_cmmd = \"typo\"\n[budget.attempt]\ntokenz = 5\n")
	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	for _, k := range []string{"check_cmmd", "budget.attempt.tokenz"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q does not name %q", err, k)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name, toml, want string
	}{
		{"max_parallel", "max_parallel = 0", "max_parallel"},
		{"max_attempts", "max_attempts = 0", "max_attempts"},
		{"start_children_on", `start_children_on = "merged"`, "start_children_on"},
		{"runner kind", "[runner]\nkind = \"gpt\"", "runner.kind"},
		{"fake needs script", "[runner]\nkind = \"fake\"", "fake_script"},
		{"host kind", "[host]\nkind = \"tmux\"", "host.kind"},
		{"count", "[budget]\ncount = \"some\"", "budget.count"},
		{"negative usd", "[budget.task]\nusd = -1.0", "budget.task"},
		{"negative size tokens", "[budget.size.S]\nattempt = { tokens = -5 }", "budget.size.S.attempt"},
		{"unknown size", "[budget.size.XL]\nattempt = { usd = 1.0 }", "budget.size.XL"},
		{"empty model", "[sizes.S]\nmodel = \"\"", "sizes.S.model"},
		{"empty base", `base_branch = ""`, "base_branch"},
		{"bad toml", "max_parallel = ", "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.toml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, err := Parse("max_parallel = 0\nmax_attempts = 0\n")
	if err == nil || !strings.Contains(err.Error(), "max_parallel") || !strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(`check_cmd = "make check"`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.CheckCmd != "make check" {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.toml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestWorktreeTimeoutAndEnvSettings(t *testing.T) {
	cfg, err := Parse(`
worktree_root = "~/wt"
gate_timeout = "90s"
env_allow = ["GOPROXY", "NPM_TOKEN"]
[runner]
claude_bin = "/opt/claude"
attempt_timeout = "1h"
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GateTimeout.Duration != 90*time.Second || cfg.Runner.AttemptTimeout.Duration != time.Hour {
		t.Fatalf("durations = %v, %v", cfg.GateTimeout, cfg.Runner.AttemptTimeout)
	}
	if cfg.Runner.ClaudeBin != "/opt/claude" || len(cfg.EnvAllow) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	got := cfg.Worktrees("/src/myrepo", "/home/u")
	if filepath.Dir(got) != "/home/u/wt" || !strings.HasPrefix(filepath.Base(got), "myrepo-") {
		t.Fatalf("Worktrees = %q", got)
	}
}

func TestWorktreesDefaultAndUnique(t *testing.T) {
	cfg := Default()
	a := cfg.Worktrees("/src/a/repo", "/home/u")
	b := cfg.Worktrees("/src/b/repo", "/home/u")
	if filepath.Dir(a) != "/home/u/.cache/mill/worktrees" {
		t.Fatalf("default worktree root = %q", a)
	}
	if a == b {
		t.Fatal("two repos with the same name share a worktree directory")
	}
	if a != cfg.Worktrees("/src/a/repo", "/home/u") {
		t.Fatal("worktree directory is not stable")
	}
}

func TestTimeoutAndEnvValidation(t *testing.T) {
	tests := []struct{ name, toml, want string }{
		{"bad duration", `gate_timeout = "soon"`, "parse config"},
		{"zero gate timeout", `gate_timeout = "0s"`, "gate_timeout"},
		{"zero attempt timeout", "[runner]\nattempt_timeout = \"0s\"", "attempt_timeout"},
		{"empty claude bin", "[runner]\nclaude_bin = \"\"", "claude_bin"},
		{"bad env key", `env_allow = ["NOT-VALID"]`, "env_allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.toml); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
