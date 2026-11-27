package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/price"
)

func TestDefaultTOMLMatchesDefault(t *testing.T) {
	t.Parallel()
	cfg, err := Parse(DefaultTOML)
	if err != nil {
		t.Fatalf("DefaultTOML does not parse: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("DefaultTOML and Default() differ:\n toml: %+v\n code: %+v", cfg, Default())
	}
}

func TestEmptyFileGivesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("empty config = %+v, want defaults", cfg)
	}
}

func TestOverrides(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
			_, err := Parse(tt.toml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	t.Parallel()
	_, err := Parse("max_parallel = 0\nmax_attempts = 0\n")
	if err == nil || !strings.Contains(err.Error(), "max_parallel") || !strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	tests := []struct{ name, toml, want string }{
		{"bad duration", `gate_timeout = "soon"`, "parse config"},
		{"zero gate timeout", `gate_timeout = "0s"`, "gate_timeout"},
		{"zero attempt timeout", "[runner]\nattempt_timeout = \"0s\"", "attempt_timeout"},
		{"zero api timeout", "[runner]\napi_timeout = \"0s\"", "runner.api_timeout must be > 0"},
		{"empty claude bin", "[runner]\nclaude_bin = \"\"", "claude_bin"},
		{"bad env key", `env_allow = ["NOT-VALID"]`, "env_allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tt.toml); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPricesOverridesParseAndValidate(t *testing.T) {
	t.Parallel()
	cfg, err := Parse(`
[prices.opus]
input = 1.5
output = 2
cache_write_5m = 3
cache_write_1h = 4
cache_read = 0.5
`)
	if err != nil {
		t.Fatal(err)
	}
	want := price.Rates{Input: 1.5, Output: 2, CacheWrite5m: 3, CacheWrite1h: 4, CacheRead: 0.5}
	if cfg.Prices["opus"] != want {
		t.Fatalf("prices = %+v", cfg.Prices)
	}
	if len(Default().Prices) != 0 {
		t.Fatal("Default() has price overrides")
	}
	if _, err := Parse("[prices.opus]\ncache_read = -1\n"); err == nil || !strings.Contains(err.Error(), "prices.opus.cache_read") {
		t.Fatalf("negative price: %v", err)
	}
	if _, err := Parse("[prices.opus]\nbogus = 1\n"); err == nil {
		t.Fatal("unknown price key accepted")
	}
}

func TestOwnsPolicyDefaultsToWarnAndIsValidated(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil || cfg.OwnsPolicy != "warn" {
		t.Fatalf("default = %q, %v", cfg.OwnsPolicy, err)
	}
	for _, v := range []string{"enforce", "off"} {
		cfg, err := Parse(`owns_policy = "` + v + `"`)
		if err != nil || cfg.OwnsPolicy != v {
			t.Fatalf("%s: %q, %v", v, cfg.OwnsPolicy, err)
		}
	}
	if _, err := Parse(`owns_policy = "strict"`); err == nil || !strings.Contains(err.Error(), "owns_policy") {
		t.Fatalf("err = %v", err)
	}
}

func TestGateTriageSettingsDefaultOnAndAreBooleans(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil || !cfg.GateRerun || !cfg.GateCheckBase {
		t.Fatalf("defaults: rerun=%v check_base=%v, %v", cfg.GateRerun, cfg.GateCheckBase, err)
	}
	cfg, err = Parse("gate_rerun = false\ngate_check_base = false\n")
	if err != nil || cfg.GateRerun || cfg.GateCheckBase {
		t.Fatalf("overrides: rerun=%v check_base=%v, %v", cfg.GateRerun, cfg.GateCheckBase, err)
	}
	for _, text := range []string{`gate_rerun = "yes"`, `gate_check_base = 1`} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("%s accepted", text)
		}
	}
}

func TestCleanOnLandDefaultsToAllAndIsValidated(t *testing.T) {
	t.Parallel()
	if got := Default().CleanOnLand; got != "all" {
		t.Errorf("default clean_on_land = %q, want all", got)
	}
	for _, v := range []string{"all", "local", "off"} {
		cfg, err := Parse(`clean_on_land = "` + v + `"`)
		if err != nil || cfg.CleanOnLand != v {
			t.Errorf("clean_on_land = %q: cfg %q, err %v", v, cfg.CleanOnLand, err)
		}
	}
	if _, err := Parse(`clean_on_land = "remote"`); err == nil || !strings.Contains(err.Error(), "clean_on_land") {
		t.Errorf("invalid clean_on_land accepted: %v", err)
	}
}

func TestModelsDefaultMapping(t *testing.T) {
	t.Parallel()
	cfg := Default()
	want := map[[2]string]string{
		{"S", "low"}: "haiku", {"S", "medium"}: "sonnet", {"S", "high"}: "opus",
		{"M", "low"}: "sonnet", {"M", "medium"}: "opus", {"M", "high"}: "opus",
	}
	for k, w := range want {
		got, why := cfg.TaskModel(k[0], k[1], "")
		if got != w || why != "mapping "+k[0]+"/"+k[1] {
			t.Errorf("TaskModel(%s, %s) = %q, %q; want %q", k[0], k[1], got, why, w)
		}
	}
	if cfg.Model("S") != "sonnet" || cfg.Model("M") != "opus" {
		t.Errorf("Model() does not return the medium mapping")
	}
}

func TestModelsMissingEntryNamesKeyToAdd(t *testing.T) {
	t.Parallel()
	got, why := Default().TaskModel("L", "high", "")
	if got != "" || !strings.Contains(why, "models") || !strings.Contains(why, "L") || !strings.Contains(why, "high") {
		t.Fatalf("TaskModel(L, high) = %q, %q", got, why)
	}
}

func TestModelsOverrideWins(t *testing.T) {
	t.Parallel()
	got, why := Default().TaskModel("S", "low", "claude-opus-5-5")
	if got != "claude-opus-5-5" || why != "override" {
		t.Fatalf("got %q, %q", got, why)
	}
}

func TestSizesFallbackMapsEveryComplexity(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("[sizes.S]\nmodel = \"x\"\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, cx := range Complexities {
		got, why := cfg.TaskModel("S", cx, "")
		if got != "x" || why != "mapping S/"+cx+" (sizes fallback)" {
			t.Errorf("TaskModel(S, %s) = %q, %q", cx, got, why)
		}
	}
	if got, why := cfg.TaskModel("M", "low", ""); got != "" || !strings.Contains(why, "sizes.M") {
		t.Errorf("TaskModel(M, low) = %q, %q; want no model and a hint", got, why)
	}
}

func TestModelsSectionIgnoresSizes(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("[sizes.S]\nmodel = \"x\"\n[models.S]\nlow = \"y\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, why := cfg.TaskModel("S", "low", ""); got != "y" || why != "mapping S/low" {
		t.Fatalf("TaskModel(S, low) = %q, %q", got, why)
	}
	if got, _ := cfg.TaskModel("S", "medium", ""); got != "" {
		t.Fatalf("TaskModel(S, medium) = %q, want no mapping (sizes ignored, defaults replaced)", got)
	}
}

func TestModelsValidation(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, toml, want string }{
		{"unknown complexity", "[models.S]\nextreme = \"opus\"", "models.S.extreme"},
		{"empty model", "[models.S]\nlow = \"\"", "models.S.low"},
		{"unknown size", "[models.XL]\nlow = \"opus\"", "models.XL"},
		{"empty tiers", "model_tiers = []", "model_tiers"},
		{"duplicate tiers", "model_tiers = [\"a\", \"a\"]", "model_tiers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tt.toml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestRolesConfigDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Roles.Planner != "opus" || cfg.Roles.VerifierMin != "sonnet" {
		t.Fatalf("roles = %+v", cfg.Roles)
	}
	cfg, err := Parse("[roles]\nplanner = \"sonnet\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Planner != "sonnet" || cfg.Roles.VerifierMin != "sonnet" {
		t.Fatalf("roles = %+v", cfg.Roles)
	}
	for _, key := range []string{"planner", "verifier_min"} {
		_, err := Parse("[roles]\n" + key + " = \"\"\n")
		if err == nil || !strings.Contains(err.Error(), "roles."+key) {
			t.Errorf("empty roles.%s: err = %v", key, err)
		}
	}
}

func TestTierFindsFirstContainedName(t *testing.T) {
	t.Parallel()
	cfg := Default()
	for model, want := range map[string]int{"haiku": 0, "sonnet": 1, "claude-opus-5-5": 2, "gpt-x": -1} {
		if got := cfg.Tier(model); got != want {
			t.Errorf("Tier(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestEscalateMovesUpTiersCappedAtTop(t *testing.T) {
	t.Parallel()
	cfg := Default()
	tests := []struct {
		model string
		steps int
		want  string
	}{
		{"haiku", 1, "sonnet"},
		{"sonnet", 5, "opus"},
		{"claude-opus-5-5", 1, "claude-opus-5-5"},
		{"gpt-x", 2, "gpt-x"},
		{"haiku", 0, "haiku"},
	}
	for _, tt := range tests {
		if got := cfg.Escalate(tt.model, tt.steps); got != tt.want {
			t.Errorf("Escalate(%q, %d) = %q, want %q", tt.model, tt.steps, got, tt.want)
		}
	}
}

func TestVerifierModelIsHigherOfImplAndMinimum(t *testing.T) {
	t.Parallel()
	cfg := Default()
	for impl, want := range map[string]string{"haiku": "sonnet", "sonnet": "sonnet", "opus": "opus", "gpt-x": "gpt-x"} {
		if got := cfg.VerifierModel(impl); got != want {
			t.Errorf("VerifierModel(%q) = %q, want %q", impl, got, want)
		}
	}
}

func TestDefaultTOMLHasModelsAndRoles(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"[models]", "[roles]", "model_tiers"} {
		if !strings.Contains(DefaultTOML, s) {
			t.Errorf("DefaultTOML lacks %s", s)
		}
	}
	if strings.Contains(DefaultTOML, "\n[sizes.") {
		t.Error("DefaultTOML still has [sizes.*] blocks")
	}
}

func TestMaxParallelGatesDefaultsToOne(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MaxParallelGates != 1 {
		t.Fatalf("MaxParallelGates = %d, want 1", cfg.MaxParallelGates)
	}
	cfg, err = Parse("max_parallel_gates = 3")
	if err != nil || cfg.MaxParallelGates != 3 {
		t.Fatalf("Parse(max_parallel_gates = 3) = %d, %v; want 3", cfg.MaxParallelGates, err)
	}
}

func TestVerifierShareDefaultsToQuarter(t *testing.T) {
	t.Parallel()
	if got := Default().Budget.VerifierShare; got != 0.25 {
		t.Fatalf("default verifier_share = %v, want 0.25", got)
	}
}

func TestVerifierBudgetScalesAttemptLimits(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if got := cfg.VerifierBudget("M"); got != (budget.Limits{USD: 0.75, Tokens: 150_000}) {
		t.Fatalf("VerifierBudget(M) = %+v, want {USD: 0.75, Tokens: 150000}", got)
	}
}

func TestVerifierBudgetKeepsUnlimitedUnitZeroAndRoundsTokensUp(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("[budget.attempt]\nusd = 0\ntokens = 1001\n")
	if err != nil {
		t.Fatal(err)
	}
	// Size L has no entry, so it uses the attempt limits: no USD limit, 1001 tokens.
	if got := cfg.VerifierBudget("L"); got != (budget.Limits{USD: 0, Tokens: 251}) {
		t.Fatalf("VerifierBudget(L) = %+v, want {USD: 0, Tokens: 251} (250.25 rounded up)", got)
	}
}

func TestVerifierShareOutsideZeroToOneIsRejected(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"0", "-0.1", "1.5"} {
		_, err := Parse("[budget]\nverifier_share = " + v + "\n")
		if err == nil || !strings.Contains(err.Error(), "budget.verifier_share") || !strings.Contains(err.Error(), v) {
			t.Errorf("verifier_share = %s: err = %v, want a budget.verifier_share error naming the value", v, err)
		}
	}
}

func TestVerifierShareOneIsAccepted(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("[budget]\nverifier_share = 1\n")
	if err != nil || cfg.Budget.VerifierShare != 1 {
		t.Fatalf("verifier_share = 1: share %v, err %v", cfg.Budget.VerifierShare, err)
	}
}

func TestDefaultTOMLDocumentsVerifierShareUnderBudget(t *testing.T) {
	t.Parallel()
	line := "verifier_share = 0.25   # share of a task size's attempt limits reserved before the verifier runs (0 < share <= 1)"
	budgetAt := strings.Index(DefaultTOML, "\n[budget]\n")
	lineAt := strings.Index(DefaultTOML, line)
	nextAt := strings.Index(DefaultTOML, "\n[budget.attempt]")
	if budgetAt < 0 || lineAt < budgetAt || lineAt > nextAt {
		t.Fatalf("DefaultTOML lacks %q under [budget]", line)
	}
}

func TestMaxParallelGatesBelowOneIsRejected(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"0", "-1"} {
		_, err := Parse("max_parallel_gates = " + v)
		if err == nil || !strings.Contains(err.Error(), "max_parallel_gates must be >= 1, got "+v) {
			t.Errorf("max_parallel_gates = %s: err = %v, want a clear >= 1 error", v, err)
		}
	}
}

func TestGateNiceDefaultsByPlatform(t *testing.T) {
	t.Parallel()
	want := 0
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		want = 10
	}
	cfg, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GateNice != want {
		t.Fatalf("default gate_nice = %d on %s, want %d", cfg.GateNice, runtime.GOOS, want)
	}
}

func TestGateNiceParsesZeroAndPositive(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 5, 19} {
		cfg, err := Parse(fmt.Sprintf("gate_nice = %d", n))
		if err != nil {
			t.Fatalf("gate_nice = %d: %v", n, err)
		}
		if cfg.GateNice != n {
			t.Fatalf("gate_nice = %d parsed as %d", n, cfg.GateNice)
		}
	}
}

func TestGateNiceNegativeRejected(t *testing.T) {
	t.Parallel()
	_, err := Parse("gate_nice = -1")
	if err == nil || !strings.Contains(err.Error(), "gate_nice must be >= 0") {
		t.Fatalf("gate_nice = -1: err = %v, want a gate_nice validation error", err)
	}
}

func TestImplementerRunsCheckDefaultsFalseAndParses(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil || cfg.ImplementerRunsCheck {
		t.Fatalf("default implementer_runs_check = %v, %v; want false", cfg.ImplementerRunsCheck, err)
	}
	cfg, err = Parse(DefaultTOML)
	if err != nil || cfg.ImplementerRunsCheck {
		t.Fatalf("DefaultTOML implementer_runs_check = %v, %v; want false", cfg.ImplementerRunsCheck, err)
	}
	cfg, err = Parse("implementer_runs_check = true\n")
	if err != nil || !cfg.ImplementerRunsCheck {
		t.Fatalf("implementer_runs_check = true parsed as %v, %v", cfg.ImplementerRunsCheck, err)
	}
	if _, err := Parse(`implementer_runs_check = "yes"`); err == nil {
		t.Fatal(`implementer_runs_check = "yes" parsed; want a type error`)
	}
}

func TestMaxInFlightDefaultsToAgentsPlusGates(t *testing.T) {
	t.Parallel()
	if got := Default().MaxInFlight; got != 3 {
		t.Fatalf("Default().MaxInFlight = %d, want 3 (max_parallel 2 + max_parallel_gates 1)", got)
	}
	cfg, err := Parse("max_parallel = 4\nmax_parallel_gates = 2\n")
	if err != nil || cfg.MaxInFlight != 6 {
		t.Fatalf("Parse(max_parallel = 4, max_parallel_gates = 2) MaxInFlight = %d, %v; want 6", cfg.MaxInFlight, err)
	}
	cfg, err = Parse("max_parallel = 2\nmax_in_flight = 5\n")
	if err != nil || cfg.MaxInFlight != 5 {
		t.Fatalf("Parse(max_in_flight = 5) = %d, %v; want 5", cfg.MaxInFlight, err)
	}
	cfg, err = Parse("max_parallel = 2\nmax_in_flight = 2\n")
	if err != nil || cfg.MaxInFlight != 2 {
		t.Fatalf("Parse(max_in_flight = max_parallel) = %d, %v; want 2", cfg.MaxInFlight, err)
	}
}

func TestMaxInFlightBelowMaxParallelIsRejected(t *testing.T) {
	t.Parallel()
	_, err := Parse("max_parallel = 3\nmax_in_flight = 2\n")
	if err == nil || !strings.Contains(err.Error(), "max_in_flight must be >= max_parallel (3), got 2") {
		t.Fatalf("err = %v, want a max_in_flight >= max_parallel error", err)
	}
}

func TestHookSettingsDefaultToNoHooksAndSixtySecondTimeout(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OnPublish != "" || cfg.OnUpdate != "" || cfg.HookEnv != nil {
		t.Fatalf("hooks = %q, %q, %v; want none", cfg.OnPublish, cfg.OnUpdate, cfg.HookEnv)
	}
	if cfg.HookTimeout.Duration != 60*time.Second {
		t.Fatalf("hook_timeout = %v, want 60s", cfg.HookTimeout)
	}
}

func TestHookSettingsLoadFromConfigFile(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	text := `on_publish = "make notify"
on_update = "./hooks/update.sh"
hook_env = ["SLACK_WEBHOOK", "_TOKEN2"]
hook_timeout = "5m"
`
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OnPublish != "make notify" || cfg.OnUpdate != "./hooks/update.sh" {
		t.Fatalf("on_publish, on_update = %q, %q", cfg.OnPublish, cfg.OnUpdate)
	}
	if !reflect.DeepEqual(cfg.HookEnv, []string{"SLACK_WEBHOOK", "_TOKEN2"}) {
		t.Fatalf("hook_env = %v", cfg.HookEnv)
	}
	if cfg.HookTimeout.Duration != 5*time.Minute {
		t.Fatalf("hook_timeout = %v, want 5m", cfg.HookTimeout)
	}
}

func TestHookTimeoutMustBePositiveInConfigFile(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"0s", "-1m"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(`hook_timeout = "`+v+`"`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), "hook_timeout") {
			t.Errorf("hook_timeout = %s: Load err = %v, want a hook_timeout error", v, err)
		}
	}
}

func TestHookEnvRejectsInvalidNamesNamingEntryAndKey(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"NOT-VALID", "1LEADING_DIGIT", "HAS SPACE", ""} {
		_, err := Parse("hook_env = [\"" + name + "\"]")
		if err == nil || !strings.Contains(err.Error(), "hook_env") || !strings.Contains(err.Error(), strconv.Quote(name)) {
			t.Errorf("hook_env entry %q: err = %v, want an error naming the key and the entry", name, err)
		}
	}
}

func TestHookEnvAcceptsLettersDigitsAndUnderscores(t *testing.T) {
	t.Parallel()
	if _, err := Parse(`hook_env = ["A", "_b", "C_9"]`); err != nil {
		t.Fatalf("valid hook_env rejected: %v", err)
	}
}

func TestAPITimeoutDefaultAndLoad(t *testing.T) {
	t.Parallel()
	if got := Default().Runner.APITimeout.Duration; got != 15*time.Minute {
		t.Fatalf("default api_timeout = %v, want 15m", got)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[runner]\napi_timeout = \"2m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.Runner.APITimeout.Duration != 2*time.Minute {
		t.Fatalf("Load = %v, %v", cfg.Runner.APITimeout, err)
	}
	if err := os.WriteFile(p, []byte("[runner]\napi_timeout = \"0s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "runner.api_timeout") {
		t.Fatalf("Load err = %v, want runner.api_timeout", err)
	}
}
