// Package planner turns a goal into a plan with a planner agent (PL1, PL4): it surveys the
// repository, prompts the agent for a plan matching PlanSchema, and validates the result.
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mill/internal/budget"
	"mill/internal/gitx"
	"mill/internal/plan"
	"mill/internal/runner"
)

// Survey limits.
const (
	maxSurveyFiles  = 400
	maxReadmeLines  = 80
	surveyNoChecker = "(none configured)"
)

// buildFiles are the common build files Survey looks for at the repository root.
var buildFiles = []string{
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py", "requirements.txt",
	"Makefile", "CMakeLists.txt", "build.gradle", "pom.xml", "Gemfile", "composer.json",
	"mix.exs", "deno.json", "justfile",
}

// Tools are the read-only tools the planner may use.
var Tools = []string{"Read", "Glob", "Grep"}

// Survey returns a short text survey of the repository at root for the planner (P2): the
// tracked files, the head of README.md, the build files present and the check command.
func Survey(ctx context.Context, root string, checkCmd string) (string, error) {
	out, err := gitx.Run(ctx, root, "ls-files")
	if err != nil {
		return "", fmt.Errorf("survey: list tracked files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Tracked files (%d)\n", len(files))
	shown := files
	if len(shown) > maxSurveyFiles {
		shown = shown[:maxSurveyFiles]
	}
	for _, f := range shown {
		fmt.Fprintln(&b, f)
	}
	if n := len(files) - len(shown); n > 0 {
		fmt.Fprintf(&b, "... and %d more files not listed\n", n)
	}
	if len(files) == 0 {
		fmt.Fprintln(&b, "(no tracked files)")
	}

	fmt.Fprintln(&b, "\n## Build files at the root")
	var present []string
	for _, name := range buildFiles {
		if st, err := os.Stat(filepath.Join(root, name)); err == nil && !st.IsDir() {
			present = append(present, name)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("survey: %w", err)
		}
	}
	if len(present) == 0 {
		fmt.Fprintln(&b, "(none)")
	} else {
		fmt.Fprintln(&b, strings.Join(present, ", "))
	}

	fmt.Fprintln(&b, "\n## Check command")
	if strings.TrimSpace(checkCmd) == "" {
		fmt.Fprintln(&b, surveyNoChecker)
	} else {
		fmt.Fprintln(&b, checkCmd)
	}

	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("survey: read README.md: %w", err)
	default:
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		fmt.Fprintf(&b, "\n## README.md (first %d lines)\n", maxReadmeLines)
		if len(lines) > maxReadmeLines {
			lines = lines[:maxReadmeLines]
		}
		fmt.Fprintln(&b, strings.Join(lines, "\n"))
	}
	return b.String(), nil
}

// PlanSchema is the JSON Schema of the plan format of internal/plan.
var PlanSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["goal", "assumptions", "tasks"],
  "properties": {
    "goal": {"type": "string", "minLength": 1},
    "assumptions": {"type": "array", "items": {"type": "string"}},
    "tasks": {
      "type": "array",
      "minItems": 1,
      "maxItems": 12,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "title", "kind", "spec", "acceptance", "checks", "owns", "depends_on", "size"],
        "properties": {
          "id": {"type": "string", "pattern": "^[a-z0-9][a-z0-9-]{0,62}$"},
          "title": {"type": "string", "minLength": 1},
          "kind": {"type": "string", "enum": ["implement", "docs", "test"]},
          "spec": {"type": "string", "minLength": 1},
          "acceptance": {"type": "array", "minItems": 1, "items": {"type": "string"}},
          "checks": {"type": "array", "items": {"type": "string"}},
          "owns": {"type": "array", "items": {"type": "string"}},
          "depends_on": {"type": "array", "items": {"type": "string"}},
          "size": {"type": "string", "enum": ["S", "M"]}
        }
      }
    }
  }
}`)

// Prompt returns the planner's instructions for goal. When problems is not empty, the
// previous plan was invalid and the prompt asks for a corrected one.
func Prompt(goal, survey string, problems []string) string {
	var b strings.Builder
	b.WriteString(`You are the planner. Break the goal below into a plan of tasks that coding agents will
implement one at a time, each on its own branch, each reviewed independently.

Rules for the plan:
- 1 to 12 tasks. Each task is one reviewable change of size S (small) or M (medium); split
  anything larger.
- kind is "implement", "docs" or "test".
- id is short kebab-case (lowercase letters, digits, dashes), unique in the plan.
- spec says exactly what to build; acceptance lists criteria a reviewer can check one by one.
- checks are shell commands run from the repository root that must pass when the task is done.
  Every implement task has at least one check (use the project's check command when it fits).
- owns lists the files the task may change, as file globs (e.g. "internal/foo/**", "README.md").
- depends_on lists only tasks whose code this task needs; leave it empty otherwise.
- Tasks that do not depend on each other own disjoint files where possible, so they can run
  in parallel.
- assumptions lists what you assumed where the goal is ambiguous.
- goal restates the goal in one line.

You may read the repository with the Read, Glob and Grep tools. Do not change anything.
Answer only with the plan as JSON matching the schema.

# Goal
`)
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n# Repository survey\n")
	b.WriteString(strings.TrimSpace(survey))
	b.WriteString("\n")
	if len(problems) > 0 {
		b.WriteString("\n# Problems in your previous plan\nYour previous plan was rejected. Fix every problem below and return the corrected plan:\n")
		for _, p := range problems {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	return b.String()
}

// Options configure Generate.
type Options struct {
	Runner         runner.Runner
	Goal           string
	Root           string // repository root; the planner runs here
	CheckCmd       string
	Model          string
	Budget         budget.Limits // per agent call
	Count          budget.Count
	LogDir         string
	ProtectedPaths []string
	Env            []string
	Timeout        time.Duration // per agent call; 0 means no limit
}

// Result is what Generate produced.
type Result struct {
	Plan  plan.Plan
	Raw   json.RawMessage // the last plan the agent returned
	Usage budget.Usage    // total over all calls
	Calls int
}

// InvalidPlanError reports a plan that was still invalid after the retry.
type InvalidPlanError struct {
	Problems []string
}

func (e *InvalidPlanError) Error() string {
	return "the planner's plan is invalid after a retry:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Generate asks the planner agent for a plan and validates it. An invalid plan goes back to
// the agent once with its problems (PL4). When the second plan is invalid too, the error is
// an *InvalidPlanError and Result.Raw holds the last plan.
func Generate(ctx context.Context, o Options) (Result, error) {
	var res Result
	survey, err := Survey(ctx, o.Root, o.CheckCmd)
	if err != nil {
		return res, err
	}
	var problems []string
	for call := 1; call <= 2; call++ {
		logDir := ""
		if o.LogDir != "" {
			logDir = filepath.Join(o.LogDir, fmt.Sprintf("call%d", call))
			if err := os.MkdirAll(logDir, 0o755); err != nil {
				return res, err
			}
		}
		out, usage, err := runOnce(ctx, o, call, Prompt(o.Goal, survey, problems), logDir)
		res.Calls++
		res.Usage = res.Usage.Add(usage)
		if err != nil {
			return res, err
		}
		res.Raw = out
		p, perr := plan.Parse(out)
		if perr == nil {
			perr = p.Validate(plan.Options{})
		}
		if perr == nil {
			res.Plan = p
			return res, nil
		}
		problems = problemList(perr)
	}
	return res, &InvalidPlanError{Problems: problems}
}

// runOnce runs the planner agent once, stopping it when it crosses the budget.
// A call that outlives o.Timeout is cancelled and reported as a timeout.
func runOnce(ctx context.Context, o Options, call int, promptText, logDir string) (json.RawMessage, budget.Usage, error) {
	var rctx context.Context
	var cancel context.CancelFunc
	if o.Timeout > 0 {
		rctx, cancel = context.WithTimeout(ctx, o.Timeout)
	} else {
		rctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	var mu sync.Mutex
	var spent budget.Usage
	var over string
	res, err := o.Runner.Run(rctx, runner.Request{
		Role: runner.RolePlanner, Prompt: promptText, Dir: o.Root, Schema: PlanSchema,
		Tools: append([]string(nil), Tools...), Model: o.Model, Budget: o.Budget, Count: o.Count,
		Name: "planner", LogDir: logDir, ProtectedPaths: o.ProtectedPaths, Env: o.Env,
		OnUsage: func(d budget.Usage) {
			mu.Lock()
			defer mu.Unlock()
			spent = spent.Add(d)
			if exceeded, why := budget.Exceeded(spent, o.Budget, o.Count); exceeded && over == "" {
				over = why
				cancel()
			}
		},
	})
	mu.Lock()
	overReason := over
	mu.Unlock()
	if o.Timeout > 0 && ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && overReason == "" {
		return nil, res.Usage, fmt.Errorf("the planner timed out after %s on call %d; raise runner.attempt_timeout in the mill config to give it more time", o.Timeout, call)
	}
	if err != nil {
		return nil, res.Usage, fmt.Errorf("planner run: %w", err)
	}
	switch {
	case res.Status == runner.StatusOK:
	case res.Status == runner.StatusBudgetExceeded:
		return nil, res.Usage, fmt.Errorf("planner stopped: budget exceeded: %s", res.Error)
	case overReason != "":
		return nil, res.Usage, fmt.Errorf("planner stopped: budget exceeded: %s", overReason)
	default:
		return nil, res.Usage, fmt.Errorf("planner %s: %s", res.Status, res.Error)
	}
	if logDir != "" && len(res.Output) > 0 {
		if err := os.WriteFile(filepath.Join(logDir, "planner.output.json"), res.Output, 0o644); err != nil {
			return nil, res.Usage, err
		}
	}
	return res.Output, res.Usage, nil
}

func problemList(err error) []string {
	var out []string
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			out = append(out, e.Error())
		}
		return out
	}
	return []string{err.Error()}
}
