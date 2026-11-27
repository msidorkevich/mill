package planner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/runner"
)

func transientStep(msg string, usd float64) runner.Step {
	return runner.Step{Error: msg, Transient: true, Usage: []budget.Usage{{CostUSD: usd}}}
}

func TestTransientFailureIsRetriedAndReturnsPlan(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {transientStep("Overloaded", 0.5), step(validPlan, 0.25)}})
	var log bytes.Buffer
	o := options(t, f)
	o.Log = &log
	res, err := Generate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Goal != "Add greeting" || res.Calls != 2 || res.Usage.CostUSD != 0.75 {
		t.Errorf("result = %+v", res)
	}
	if got, want := log.String(), "planner: Overloaded; retrying\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	if calls[0].Prompt != calls[1].Prompt {
		t.Error("the retry changed the prompt")
	}
	if calls[0].LogDir == calls[1].LogDir {
		t.Errorf("the retry overwrites the first run's logs in %s", calls[0].LogDir)
	}
	if _, err := os.Stat(calls[1].LogDir); err != nil {
		t.Error(err)
	}
}

func TestTransientFailureTwiceFailsAfterTwoRuns(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {
		transientStep("Overloaded", 0), transientStep("Request timed out", 0), step(validPlan, 0)}})
	var log bytes.Buffer
	o := options(t, f)
	o.Log = &log
	res, err := Generate(context.Background(), o)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"Request timed out", "run `mill plan` again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(f.Calls()) != 2 || res.Calls != 2 {
		t.Errorf("calls = %d/%d, want 2", res.Calls, len(f.Calls()))
	}
	if n := strings.Count(log.String(), "retrying"); n != 1 {
		t.Errorf("%d retry lines: %q", n, log.String())
	}
}

func TestNonTransientFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	f := runner.NewFake(runner.Script{runner.RolePlanner: {{Error: "boom"}, step(validPlan, 0)}})
	var log bytes.Buffer
	o := options(t, f)
	o.Log = &log
	_, err := Generate(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls()) != 1 || log.Len() != 0 {
		t.Errorf("calls = %d, log = %q", len(f.Calls()), log.String())
	}
}

func TestPromptRulesForBehaviouralDepsAndHelperPrefixes(t *testing.T) {
	t.Parallel()
	p := strings.Join(strings.Fields(Prompt("goal", "survey", nil)), " ")
	for _, want := range []string{
		"when a task's tests exercise a command or behaviour that another task in the plan changes, make it depend on that task, even if they share no files",
		"Test helpers added to a package shared with sibling tasks get a task-specific prefix",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
