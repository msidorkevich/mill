package prompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/taskspec"
)

// ResolverSchema is the JSON Schema the resolver's final output must match.
var ResolverSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "How you resolved the conflicts, in a few sentences."},
    "resolved": {"type": "array", "items": {"type": "string"}, "description": "The conflicted files you resolved."}
  },
  "required": ["summary", "resolved"],
  "additionalProperties": false
}`)

// ResolverOutput is the resolver's structured result.
type ResolverOutput struct {
	Summary  string   `json:"summary"`
	Resolved []string `json:"resolved"`
}

// ParseResolver decodes and checks the resolver's output.
func ParseResolver(raw json.RawMessage) (ResolverOutput, error) {
	var o ResolverOutput
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("resolver output is not valid JSON: %w", err)
	}
	if strings.TrimSpace(o.Summary) == "" {
		return o, errors.New("resolver output has no summary")
	}
	return o, nil
}

// ConflictFile is one file with conflicts and its conflict hunks (not the whole file).
type ConflictFile struct {
	Path  string
	Hunks string
}

// BaseCommit is one commit of the new base that the task's changes are replayed onto.
type BaseCommit struct {
	SHA      string
	Subject  string
	Trailers string
}

// ResolverInput is everything the resolver prompt is built from (P2: only what it needs).
type ResolverInput struct {
	Task        taskspec.Spec
	Conflicts   []ConflictFile
	BaseCommits []BaseCommit
	Onto        string // the branch or commit the task is being restacked onto
}

// Resolver builds the resolver prompt.
func Resolver(in ResolverInput) string {
	var b strings.Builder
	t := in.Task
	onto := in.Onto
	if onto == "" {
		onto = "its new base"
	}
	fmt.Fprintf(&b, "You are resolving merge conflicts in this git worktree (your current directory). mill is replaying task %s onto %s and the replay stopped on conflicts.\n\n", t.ID, onto)
	fmt.Fprintf(&b, "# Task %s: %s\n\nKind: %s\n\n## Spec\n%s\n\n", t.ID, t.Title, t.Kind, strings.TrimSpace(t.Spec))
	if len(t.Acceptance) > 0 {
		b.WriteString("## Acceptance criteria (the task's changes must still meet each one)\n")
		for _, a := range t.Acceptance {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(in.BaseCommits) > 0 {
		fmt.Fprintf(&b, "## Commits on %s that the task's changes conflict with\n", onto)
		for _, c := range in.BaseCommits {
			fmt.Fprintf(&b, "- %s %s\n", short(c.SHA), strings.TrimSpace(c.Subject))
			if tr := strings.TrimSpace(c.Trailers); tr != "" {
				fmt.Fprintf(&b, "%s\n", indentLines(tr, "    "))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("## Conflicted files\n")
	for _, c := range in.Conflicts {
		fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n", c.Path, strings.TrimRight(c.Hunks, "\n"))
	}
	b.WriteString(`## Rules
- Edit only the conflicted files listed above.
- Remove every conflict marker (<<<<<<<, =======, >>>>>>>) from each of them.
- Keep both intents: the base commits' changes and this task's changes must both survive.
- Do not run git commands that change history or the index (commit, add, rm, reset, checkout, restore --staged, rebase, merge, cherry-pick, stash): mill stages the files and continues.
- When you are done, give your final answer as the structured output: summary, resolved (the files you resolved).
`)
	return b.String()
}

func indentLines(s, pre string) string {
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre)
}
