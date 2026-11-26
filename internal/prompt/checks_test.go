package prompt

import (
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/taskspec"
)

// agentPrompts builds the implementer, fixer and resolver prompts for a task with one check,
// with check_cmd "make check" and the given implementer_runs_check setting.
func agentPrompts(runsCheck bool) map[string]string {
	t := taskspec.Spec{ID: "g/greet", Title: "Greet", Kind: "implement", Spec: "make greet say hello",
		Checks: []string{"go test ./greet/ -run 'Greet'"}}
	return map[string]string{
		"implementer": Implementer(ImplementerInput{Task: t, CheckCmd: "make check", RunsCheck: runsCheck, Attempt: 1, MaxAttempts: 3}),
		"fixer": Implementer(ImplementerInput{Task: t, CheckCmd: "make check", RunsCheck: runsCheck, Attempt: 2, MaxAttempts: 3,
			Findings: []Finding{{Source: "gate", Issue: "`make check` failed"}}}),
		"resolver": Resolver(ResolverInput{Task: t, CheckCmd: "make check", RunsCheck: runsCheck,
			Conflicts: []ConflictFile{{Path: "greet.go", Hunks: "<<<<<<< HEAD\n=======\n>>>>>>> x\n"}}}),
	}
}

// section returns the text of the prompt section whose heading starts with heading, up to
// the next heading, or "" when there is none.
func section(p, heading string) string {
	i := strings.Index(p, "\n"+heading)
	if i < 0 {
		return ""
	}
	rest := p[i+1:]
	if j := strings.Index(rest[1:], "\n## "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

func TestDefaultPromptsListChecksAndLeaveCheckCmdToTheGate(t *testing.T) {
	t.Parallel()
	for role, p := range agentPrompts(false) {
		run := section(p, "## Run these before you finish")
		if run == "" {
			t.Fatalf("%s prompt has no 'Run these before you finish' section:\n%s", role, p)
		}
		for _, want := range []string{"`go test ./greet/ -run 'Greet'`", "`go build ./... && go vet ./...`", "`go test` of those packages", "-run"} {
			if !strings.Contains(run, want) {
				t.Errorf("%s 'Run these' section lacks %q:\n%s", role, want, run)
			}
		}
		if strings.Contains(run, "make check") {
			t.Errorf("%s prompt asks the agent to run check_cmd:\n%s", role, run)
		}
		if strings.Contains(p, "## Gate (must pass; run these yourself before you finish)") {
			t.Errorf("%s prompt has the old gate section:\n%s", role, p)
		}
		cc := section(p, "## check_cmd")
		for _, want := range []string{"check_cmd (`make check`)", "mill runs check_cmd", "in the gate after you finish", "findings", "Do not run check_cmd yourself"} {
			if !strings.Contains(cc, want) {
				t.Errorf("%s check_cmd section lacks %q:\n%s", role, want, p)
			}
		}
	}
}

func TestImplementerRunsCheckPromptsKeepTheGateSection(t *testing.T) {
	t.Parallel()
	for role, p := range agentPrompts(true) {
		gate := section(p, "## Gate (must pass; run these yourself before you finish)")
		if gate == "" {
			t.Fatalf("%s prompt has no gate section:\n%s", role, p)
		}
		for _, want := range []string{"- `make check`\n", "- `go test ./greet/ -run 'Greet'`\n"} {
			if !strings.Contains(gate, want) {
				t.Errorf("%s gate section lacks %q:\n%s", role, want, gate)
			}
		}
		for _, unwanted := range []string{"## Run these before you finish", "## check_cmd", "Do not run check_cmd"} {
			if strings.Contains(p, unwanted) {
				t.Errorf("%s prompt contains %q:\n%s", role, unwanted, p)
			}
		}
	}
}

func TestAgentPromptsConfineScratchFilesAndProcessWaits(t *testing.T) {
	t.Parallel()
	for _, runsCheck := range []bool{false, true} {
		for role, p := range agentPrompts(runsCheck) {
			for _, want := range []string{"Write scratch files under $TMPDIR", "Wait only on processes you started yourself (your own background jobs)", "machine-wide process patterns"} {
				if !strings.Contains(p, want) {
					t.Errorf("%s prompt (implementer_runs_check=%v) lacks %q:\n%s", role, runsCheck, want, p)
				}
			}
		}
	}
}
