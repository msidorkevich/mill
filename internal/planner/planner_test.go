package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
)

const validPlan = `{"goal": "Add greeting", "assumptions": [], "tasks": [
  {"id": "a", "title": "A", "kind": "implement", "spec": "do a", "acceptance": ["a works"],
   "checks": ["go test ./..."], "owns": ["a.go"], "depends_on": [], "size": "S", "complexity": "medium", "complexity_reason": "ordinary logic"}]}`

const invalidPlan = `{"goal": "", "assumptions": [], "tasks": [
  {"id": "a", "title": "A", "kind": "implement", "spec": "do a", "acceptance": ["a works"],
   "checks": [], "owns": ["a.go"], "depends_on": ["zzz"], "size": "S", "complexity": "medium", "complexity_reason": "ordinary logic"}]}`

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.Run(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSurveyListsFilesReadmeBuildFilesAndCheck(t *testing.T) {
	t.Parallel()
	var readme strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&readme, "readme line %d\n", i)
	}
	files := map[string]string{"README.md": readme.String(), "go.mod": "module x\n", "Makefile": "all:\n"}
	for i := 0; i < 410; i++ {
		files[fmt.Sprintf("src/f%03d.go", i)] = "package src\n"
	}
	root := gitRepo(t, files)
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Survey(context.Background(), root, "make check")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README.md\n", "go.mod\n", "src/f000.go\n", "13 more files", "go.mod, Makefile",
		"make check", "readme line 1\n", "readme line 80\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("survey missing %q:\n%s", want, s)
		}
	}
	for _, unwanted := range []string{"readme line 81\n", "untracked.txt", "package.json", "src/f409.go"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("survey contains %q", unwanted)
		}
	}
	listed := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "src/") || l == "README.md" || l == "go.mod" || l == "Makefile" {
			listed++
		}
	}
	if listed != 400 {
		t.Errorf("listed %d paths, want 400", listed)
	}
}

func TestSurveySmallRepoWithoutReadmeOrCheck(t *testing.T) {
	t.Parallel()
	root := gitRepo(t, map[string]string{"main.py": "print(1)\n"})
	s, err := Survey(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.py", "(none)", "(none configured)"} {
		if !strings.Contains(s, want) {
			t.Errorf("survey missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "more files") || strings.Contains(s, "README") {
		t.Errorf("unexpected survey:\n%s", s)
	}
}

func TestSurveyOutsideGitRepoFails(t *testing.T) {
	t.Parallel()
	if _, err := Survey(context.Background(), t.TempDir(), ""); err == nil {
		t.Fatal("survey of a non-repo succeeded")
	}
}

func TestPlanSchemaRequiresEveryField(t *testing.T) {
	t.Parallel()
	var s struct {
		Type                 string   `json:"type"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           struct {
			Tasks struct {
				Items struct {
					AdditionalProperties *bool                      `json:"additionalProperties"`
					Required             []string                   `json:"required"`
					Properties           map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(PlanSchema, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if s.Type != "object" || s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Errorf("plan: type %q, additionalProperties %v", s.Type, s.AdditionalProperties)
	}
	for _, f := range []string{"goal", "assumptions", "tasks"} {
		if !slices.Contains(s.Required, f) {
			t.Errorf("plan field %q not required", f)
		}
	}
	items := s.Properties.Tasks.Items
	if items.AdditionalProperties == nil || *items.AdditionalProperties {
		t.Error("task allows additional properties")
	}
	fields := []string{"id", "title", "kind", "spec", "acceptance", "checks", "owns", "depends_on", "size", "complexity", "complexity_reason"}
	for _, f := range fields {
		if !slices.Contains(items.Required, f) {
			t.Errorf("task field %q not required", f)
		}
		if _, ok := items.Properties[f]; !ok {
			t.Errorf("task field %q has no schema", f)
		}
	}
	if len(items.Properties) != len(fields) {
		t.Errorf("task has %d properties, want %d", len(items.Properties), len(fields))
	}
	for _, want := range []string{`"implement", "docs", "test"`, `"S", "M"`} {
		if !strings.Contains(string(PlanSchema), want) {
			t.Errorf("schema missing enum %s", want)
		}
	}
}

func TestPromptIncludesProblemsOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	p := Prompt("add a greeting", "SURVEY-TEXT", nil)
	for _, want := range []string{"add a greeting", "SURVEY-TEXT", "1 to 12", "S", "M", "at least one check", "globs", "disjoint"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "previous plan") {
		t.Error("first prompt mentions a previous plan")
	}
	p2 := Prompt("add a greeting", "SURVEY-TEXT", []string{"PROBLEM-ONE", "PROBLEM-TWO"})
	for _, want := range []string{"PROBLEM-ONE", "PROBLEM-TWO", "corrected"} {
		if !strings.Contains(p2, want) {
			t.Errorf("retry prompt missing %q", want)
		}
	}
}

func options(t *testing.T, f *runner.Fake) Options {
	root := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	return Options{Runner: f, Goal: "add a greeting", Root: root, CheckCmd: "go test ./...", Model: "opus",
		Budget: budget.Limits{USD: 3}, Count: budget.CountBillable, LogDir: t.TempDir(),
		ProtectedPaths: []string{filepath.Join(root, ".mill")}, Env: []string{"PATH=/bin"}}
}

func step(out string, usd float64) runner.Step {
	return runner.Step{Output: json.RawMessage(out), Usage: []budget.Usage{{InputTokens: 10, CostUSD: usd}}}
}

func TestGenerateReturnsValidPlanFromFirstCall(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(validPlan, 0.25)}})
	o := options(t, f)
	res, err := Generate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 1 || res.Plan.Goal != "Add greeting" || len(res.Plan.Tasks) != 1 || res.Usage.CostUSD != 0.25 {
		t.Errorf("result = %+v", res)
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	c := calls[0]
	if c.Role != runner.RolePlanner || c.Dir != o.Root || string(c.Schema) != string(PlanSchema) ||
		!slices.Equal(c.Tools, []string{"Read", "Glob", "Grep"}) || c.Model != "opus" ||
		!slices.Equal(c.ProtectedPaths, o.ProtectedPaths) || !slices.Equal(c.Env, o.Env) || c.Budget != o.Budget {
		t.Errorf("request = %+v", c)
	}
	if !strings.Contains(c.Prompt, "add a greeting") || !strings.Contains(c.Prompt, "go.mod") {
		t.Errorf("prompt lacks goal or survey:\n%s", c.Prompt)
	}
}

func TestGenerateRetriesOnceWithProblems(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(invalidPlan, 0.5), step(validPlan, 0.25)}})
	res, err := Generate(context.Background(), options(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 2 || len(res.Plan.Tasks) != 1 || res.Usage.CostUSD != 0.75 {
		t.Errorf("result = %+v", res)
	}
	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	if strings.Contains(calls[0].Prompt, "zzz") {
		t.Error("first prompt contains the problems")
	}
	for _, want := range []string{"goal must not be empty", "zzz", "an implement task needs at least one check"} {
		if !strings.Contains(calls[1].Prompt, want) {
			t.Errorf("retry prompt missing %q", want)
		}
	}
}

func TestGenerateTwoInvalidPlansFailWithProblemsAndLastPlan(t *testing.T) {
	t.Parallel()
	second := `{"goal": "x", "assumptions": [], "tasks": []}`
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(invalidPlan, 0.5), step(second, 0.5), step(validPlan, 0)}})
	res, err := Generate(context.Background(), options(t, f))
	var ie *InvalidPlanError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "no tasks") || len(ie.Problems) == 0 {
		t.Errorf("err = %v", err)
	}
	if string(res.Raw) != second || res.Calls != 2 || len(f.Calls()) != 2 || res.Usage.CostUSD != 1 {
		t.Errorf("result = %+v, calls %d", res, len(f.Calls()))
	}
}

func TestGenerateUnparsableOutputIsRetried(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(`{"goal": "x", "extra": 1}`, 0), step(validPlan, 0)}})
	res, err := Generate(context.Background(), options(t, f))
	if err != nil || res.Calls != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if !strings.Contains(f.Calls()[1].Prompt, "extra") {
		t.Errorf("retry prompt lacks the parse error:\n%s", f.Calls()[1].Prompt)
	}
}

func TestGenerateAgentErrorFails(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {{Error: "boom"}}})
	_, err := Generate(context.Background(), options(t, f))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateStopsAtBudget(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {{Output: json.RawMessage(validPlan),
		Usage: []budget.Usage{{CostUSD: 2}, {CostUSD: 2}, {CostUSD: 2}}}}})
	res, err := Generate(context.Background(), options(t, f))
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v", err)
	}
	if res.Usage.CostUSD != 4 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestGenerateTimeoutIsPerCall(t *testing.T) {
	t.Parallel()
	a, b := step(invalidPlan, 0.5), step(validPlan, 0.25)
	a.DelayMS, b.DelayMS = 150, 150
	f := runner.NewFake(runner.Script{runner.RolePlanner: {a, b}})
	o := options(t, f)
	o.Timeout = 250 * time.Millisecond
	res, err := Generate(context.Background(), o)
	if err != nil {
		t.Fatalf("second call was cut short: %v", err)
	}
	if res.Calls != 2 {
		t.Errorf("calls = %d", res.Calls)
	}
}

func TestGenerateTimeoutNamesDurationCallAndSetting(t *testing.T) {
	t.Parallel()
	slow := step(validPlan, 0.25)
	slow.DelayMS = 5000
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(invalidPlan, 0.5), slow}})
	o := options(t, f)
	o.Timeout = 100 * time.Millisecond
	res, err := Generate(context.Background(), o)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	for _, want := range []string{"timed out after 100ms", "call 2", "runner.attempt_timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if res.Usage.CostUSD != 0.5 || res.Calls != 2 {
		t.Errorf("result = %+v", res)
	}
}

func TestGenerateRateLimitedNamesLimitAndResetTime(t *testing.T) {
	t.Parallel()
	resets := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	f := runner.NewFake(runner.Script{runner.RolePlanner: {{RateLimit: &runner.RateLimit{Type: "five_hour", ResetsAt: resets}}, step(validPlan, 0)}})
	res, err := Generate(context.Background(), options(t, f))
	var rl *RateLimitedError
	if !errors.As(err, &rl) || !rl.ResetsAt.Equal(resets) {
		t.Fatalf("err = %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "rate limited: the five_hour usage limit was reached") ||
		!strings.Contains(msg, "resets at ") || !strings.Contains(msg, resets.Local().Format("15:04")) {
		t.Fatalf("err = %v", err)
	}
	if res.Calls != 1 || len(f.Calls()) != 1 {
		t.Fatalf("calls = %d/%d, want 1 (no retry after a rate limit)", res.Calls, len(f.Calls()))
	}
}

func TestComplexitySchemaRequiredWithEnum(t *testing.T) {
	t.Parallel()
	for _, want := range []string{`"complexity", "complexity_reason"]`, `"enum": ["low", "medium", "high"]`} {
		if !strings.Contains(string(PlanSchema), want) {
			t.Errorf("schema missing %s", want)
		}
	}
	if strings.Contains(string(PlanSchema), `"model"`) {
		t.Error("schema lets the planner set model")
	}
}

func TestComplexityPromptExplainsSizeVersusComplexity(t *testing.T) {
	t.Parallel()
	p := Prompt("g", "s", nil)
	for _, want := range []string{"size is the amount of code", "how hard the change is to get right",
		"Concurrency, crash recovery, security and cross-cutting state are high", "Ordinary feature logic is medium",
		"Docs, text-only changes and mechanical renames are low", "complexity_reason is one short line"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestComplexityHugeIsRejectedByValidation(t *testing.T) {
	t.Parallel()
	bad := strings.Replace(validPlan, `"complexity": "medium"`, `"complexity": "huge"`, 1)
	f := runner.NewFake(runner.Script{runner.RolePlanner: {step(bad, 0.1), step(bad, 0.1)}})
	if _, err := Generate(context.Background(), options(t, f)); err == nil {
		t.Fatal("plan with complexity huge accepted")
	}
	f = runner.NewFake(runner.Script{runner.RolePlanner: {step(validPlan, 0.1)}})
	res, err := Generate(context.Background(), options(t, f))
	if err != nil || res.Plan.Tasks[0].Complexity != "medium" {
		t.Fatalf("valid complexity: %v %+v", err, res)
	}
}

func TestPromptAsksForNarrowChecksAndFastPolling(t *testing.T) {
	t.Parallel()
	p := Prompt("goal", "survey", nil)
	for _, want := range []string{"Task checks must be narrow", "-run", "millisecond polling", "never production"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
