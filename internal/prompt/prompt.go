// Package prompt builds the implementer and verifier prompts, their output schemas, and
// parses their outputs (TP2, TP4).
//
// Each agent gets only what it needs (P2). The verifier never sees the implementer's
// transcript: it gets the task, the diff and the gate results.
package prompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// ImplementerSchema is the JSON Schema the implementer's final output must match.
var ImplementerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "What you changed and why, in a few sentences."},
    "files_changed": {"type": "array", "items": {"type": "string"}},
    "notes": {"type": "string", "description": "Anything the reviewer should know: assumptions, follow-ups, open questions."}
  },
  "required": ["summary", "files_changed", "notes"],
  "additionalProperties": false
}`)

// VerifierSchema is the JSON Schema the verifier's final output must match.
var VerifierSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["pass", "fail"]},
    "summary": {"type": "string"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "file": {"type": "string"},
          "line": {"type": "integer", "minimum": 0},
          "issue": {"type": "string"}
        },
        "required": ["file", "line", "issue"],
        "additionalProperties": false
      }
    }
  },
  "required": ["verdict", "summary", "findings"],
  "additionalProperties": false
}`)

// ImplementerOutput is the implementer's structured result.
type ImplementerOutput struct {
	Summary      string   `json:"summary"`
	FilesChanged []string `json:"files_changed"`
	Notes        string   `json:"notes"`
}

// Finding is one problem to fix in the next attempt.
type Finding struct {
	Source string `json:"source"` // gate, verifier, mill, operator
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Issue  string `json:"issue"`
}

func (f Finding) String() string {
	loc := ""
	switch {
	case f.File != "" && f.Line > 0:
		loc = fmt.Sprintf("%s:%d: ", f.File, f.Line)
	case f.File != "":
		loc = f.File + ": "
	}
	return fmt.Sprintf("[%s] %s%s", f.Source, loc, f.Issue)
}

// VerifierOutput is the verifier's structured result.
type VerifierOutput struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// ParseImplementer decodes and checks the implementer's output.
func ParseImplementer(raw json.RawMessage) (ImplementerOutput, error) {
	var o ImplementerOutput
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("implementer output is not valid JSON: %w", err)
	}
	if strings.TrimSpace(o.Summary) == "" {
		return o, errors.New("implementer output has no summary")
	}
	return o, nil
}

// ParseVerifier decodes and checks the verifier's output. A fail verdict must carry at
// least one finding; every finding must name a file and an issue (TP4).
func ParseVerifier(raw json.RawMessage) (VerifierOutput, error) {
	var o VerifierOutput
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("verifier output is not valid JSON: %w", err)
	}
	if o.Verdict != "pass" && o.Verdict != "fail" {
		return o, fmt.Errorf("verifier verdict must be pass or fail, got %q", o.Verdict)
	}
	for i := range o.Findings {
		o.Findings[i].Source = "verifier"
		if strings.TrimSpace(o.Findings[i].File) == "" || strings.TrimSpace(o.Findings[i].Issue) == "" {
			return o, fmt.Errorf("verifier finding %d must name a file and an issue", i+1)
		}
	}
	if o.Verdict == "fail" && len(o.Findings) == 0 {
		return o, errors.New("verifier verdict is fail but it gave no findings")
	}
	return o, nil
}

// ImplementerInput is everything the implementer (and fixer) prompt is built from.
type ImplementerInput struct {
	Task     taskspec.Spec
	CheckCmd string // the project gate (config check_cmd); the task's checks are in Task.Checks
	// RunsCheck (config implementer_runs_check) asks the agent to run check_cmd itself.
	RunsCheck   bool
	Attempt     int
	MaxAttempts int
	Findings    []Finding // from the previous attempt; empty on the first
}

// Implementer builds the implementer prompt.
func Implementer(in ImplementerInput) string {
	var b strings.Builder
	t := in.Task
	fmt.Fprintf(&b, "You are implementing one task in this git worktree (your current directory). Attempt %d of %d.\n\n", in.Attempt, in.MaxAttempts)
	fmt.Fprintf(&b, "# Task %s: %s\n\nKind: %s\n\n## Spec\n%s\n\n", t.ID, t.Title, t.Kind, strings.TrimSpace(t.Spec))
	if len(t.Acceptance) > 0 {
		b.WriteString("## Acceptance criteria (an independent reviewer checks each one)\n")
		for _, a := range t.Acceptance {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(t.Owns) > 0 {
		b.WriteString("## Files this task owns (stay within them unless the spec requires otherwise)\n")
		for _, o := range t.Owns {
			fmt.Fprintf(&b, "- %s\n", o)
		}
		b.WriteString("\n")
	}
	writeChecks(&b, in.CheckCmd, t.Checks, in.RunsCheck)
	if len(in.Findings) > 0 {
		b.WriteString("## Findings from the previous attempt (fix every one)\n")
		b.WriteString("Your previous changes are already committed in this worktree; build on them.\n")
		for _, f := range in.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	b.WriteString(`## Rules
- Read the surrounding code first and match its style. Implement the spec and nothing more.
- Add or update tests that prove each acceptance criterion.
- Work only inside this directory. Do not run git commit, push, reset, checkout, rebase or branch: mill commits your changes.
- Do not change files outside the task's scope to make checks pass.
`)
	b.WriteString(processRules)
	b.WriteString(`- When you are done, give your final answer as the structured output: summary, files_changed, notes.
`)
	return b.String()
}

// processRules are the rules on processes and scratch files shared by the implementer,
// fixer and resolver prompts.
const processRules = `- Wait only on processes you started yourself (your own background jobs). Never wait on, poll or kill machine-wide process patterns (pgrep, pkill, ps | grep): other agents share this machine.
- Write scratch files under $TMPDIR, never in this directory.
`

// writeChecks writes the commands the agent runs before it finishes. With runsCheck it
// lists check_cmd and the task's checks as the gate the agent must pass itself. Without it
// the agent runs the task's checks and the tests of the packages it changed, and is told
// that mill runs check_cmd in the gate, so it must not run it.
func writeChecks(b *strings.Builder, checkCmd string, checks []string, runsCheck bool) {
	checkCmd = strings.TrimSpace(checkCmd)
	if runsCheck {
		var cmds []string
		for _, c := range append([]string{checkCmd}, checks...) {
			if strings.TrimSpace(c) != "" {
				cmds = append(cmds, c)
			}
		}
		if len(cmds) > 0 {
			b.WriteString("## Gate (must pass; run these yourself before you finish)\n")
			for _, c := range cmds {
				fmt.Fprintf(b, "- `%s`\n", c)
			}
			b.WriteString("\n")
		}
		return
	}
	b.WriteString("## Run these before you finish\n")
	for _, c := range checks {
		if strings.TrimSpace(c) != "" {
			fmt.Fprintf(b, "- `%s`\n", c)
		}
	}
	b.WriteString("- The tests of the packages you changed. For Go: `go build ./... && go vet ./...`, plus `go test` of those packages (with `-run` when the change is narrow enough).\n\n")
	if checkCmd != "" {
		b.WriteString("## check_cmd (mill runs it; do not run it yourself)\n")
		fmt.Fprintf(b, "mill runs check_cmd (`%s`) in the gate after you finish and returns its failures to you as findings. Do not run check_cmd yourself.\n\n", checkCmd)
	}
}

// MaxDiffBytes bounds the diff included in the verifier prompt.
const MaxDiffBytes = 150_000

// VerifierInput is everything the verifier prompt is built from.
type VerifierInput struct {
	Task     taskspec.Spec
	BaseSHA  string
	Diff     string
	DiffStat string
	Gate     gate.Result
	// OutsideOwns lists changed files that no pattern in the task's owns matches.
	OutsideOwns []string
	// Flaky lists gate commands that failed once and passed on a re-run.
	Flaky []string
	// Siblings are the other tasks of the task's goal; empty for a standalone task.
	Siblings []SiblingTask
	// BaseChanged lists files that changed on the base branch since the task started.
	BaseChanged []string
	// BaseChangedStat is the diffstat of those base changes, shown with BaseChanged.
	BaseChangedStat string
}

// SiblingTask is another task of the same goal, shown to the verifier so it can tell which
// work belongs elsewhere.
type SiblingTask struct {
	ID      string
	Title   string
	Owns    []string
	Summary string
}

// Verifier builds the verifier prompt.
func Verifier(in VerifierInput) string {
	var b strings.Builder
	t := in.Task
	b.WriteString("You are an independent reviewer. Decide whether the change below fully implements the task. You did not write it.\n\n")
	fmt.Fprintf(&b, "# Task %s: %s\n\nKind: %s\n\n## Spec\n%s\n\n", t.ID, t.Title, t.Kind, strings.TrimSpace(t.Spec))
	if len(t.Acceptance) > 0 {
		b.WriteString("## Acceptance criteria (check every one)\n")
		for i, a := range t.Acceptance {
			fmt.Fprintf(&b, "%d. %s\n", i+1, a)
		}
		b.WriteString("\n")
	}
	if len(in.Siblings) > 0 {
		b.WriteString("## Other tasks of this goal\n")
		for _, s := range in.Siblings {
			owns := "(none)"
			if len(s.Owns) > 0 {
				owns = strings.Join(s.Owns, ", ")
			}
			fmt.Fprintf(&b, "- %s: %s\n  Owns: %s\n  Summary: %s\n", s.ID, s.Title, owns, s.Summary)
		}
		b.WriteString("\nThese tasks are done separately. Work that belongs to another task's scope is out of scope here: do not fail this task for findings about it.\n\n")
	}
	b.WriteString("## Gate results (already run by mill on this exact change)\n")
	if len(in.Gate.Commands) == 0 {
		fmt.Fprintf(&b, "No commands ran: %s\n", in.Gate.Reason)
	}
	for _, c := range in.Gate.Commands {
		status := "passed"
		if !c.Passed() {
			status = "FAILED"
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", c.Cmd, status)
	}
	b.WriteString("\n")
	if len(in.Flaky) > 0 {
		b.WriteString("## Flaky gate commands\n")
		for _, c := range in.Flaky {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		b.WriteString("\nThese commands failed once and passed on a re-run. Fail with a finding if the change itself could make them nondeterministic (for example new tests that depend on timing, ordering, shared state or the environment).\n\n")
	}
	fmt.Fprintf(&b, "## Change (diff from %s to HEAD)\n", short(in.BaseSHA))
	if len(in.Diff) > MaxDiffBytes {
		fmt.Fprintf(&b, "The diff is too large to include (%d bytes). Files changed:\n```\n%s\n```\nRead the changed files to review them.\n\n", len(in.Diff), in.DiffStat)
	} else {
		fmt.Fprintf(&b, "```diff\n%s\n```\n\n", in.Diff)
	}
	if len(in.BaseChanged) > 0 {
		b.WriteString("## Changed on the base since this task started\n")
		for _, f := range in.BaseChanged {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.TrimRight(in.BaseChangedStat, "\n"))
		b.WriteString("\nCheck that this task's change does not duplicate or contradict what landed on the base in these files (for example a second section with the same purpose). Report a finding if it does.\n\n")
	}
	if len(in.OutsideOwns) > 0 {
		b.WriteString("## Files changed outside the files the task owns\n")
		for _, f := range in.OutsideOwns {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\nThese files are outside the files the task owns. Pass only if the spec or the acceptance criteria require each of them; otherwise fail with a finding naming the file.\n\n")
	}
	b.WriteString(`## How to review
- You may read any file in this directory for context. Do not modify anything.
- Check each acceptance criterion against the code and the tests. A criterion without a test that proves it is not met.
- Look for bugs, missing edge cases, and changes outside the task's scope.
- verdict "pass" only if every criterion is met and you found no blocking problem.
- verdict "fail" needs at least one finding. Each finding names a file, a line (0 if the problem is a missing file or not tied to a line) and a concrete issue the implementer can fix.
- Give your answer as the structured output: verdict, summary, findings.
`)
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
