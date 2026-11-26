package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/taskspec"
)

func TestResolverPromptHasHunksSpecAndBaseCommitsOnly(t *testing.T) {
	t.Parallel()
	task := taskspec.Spec{ID: "g/greet", Title: "Greet", Kind: "implement", Spec: "make greet say hello",
		Acceptance: []string{"greet prints hello"}, Owns: []string{"greet.go"}}
	hunk := "<<<<<<< HEAD\n\treturn \"hi there\"\n=======\n\treturn \"hello\"\n>>>>>>> mill: g/greet attempt 1\n"
	p := Resolver(ResolverInput{
		Task:      task,
		Conflicts: []ConflictFile{{Path: "greet.go", Hunks: hunk}},
		BaseCommits: []BaseCommit{
			{SHA: "0123456789abcdef0123", Subject: "mill: g/polite attempt 2", Trailers: "Mill-Task: g/polite\nMill-Attempt: 2"},
		},
		Onto: "mill/g/polite",
	})
	for _, want := range []string{
		hunk[:len(hunk)-1], "### greet.go",
		"make greet say hello", "greet prints hello",
		"0123456789ab mill: g/polite attempt 2", "Mill-Task: g/polite", "Mill-Attempt: 2", "mill/g/polite",
		"Edit only the conflicted files", "Remove every conflict marker", "Keep both intents",
		"Do not run git commands that change history or the index",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	// Only the hunks: nothing outside them, no other task's spec, no implementer gate list.
	for _, unwanted := range []string{"package greet", "func Greet", "## Gate", "Findings from the previous attempt"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("prompt contains %q:\n%s", unwanted, p)
		}
	}
}

func TestResolverPromptOmitsOtherTasksSpecs(t *testing.T) {
	t.Parallel()
	p := Resolver(ResolverInput{
		Task:        taskspec.Spec{ID: "g/a", Title: "A", Kind: "implement", Spec: "spec of a"},
		Conflicts:   []ConflictFile{{Path: "x.go", Hunks: "<<<<<<< HEAD\nb\n=======\na\n>>>>>>> a\n"}},
		BaseCommits: []BaseCommit{{SHA: "abc", Subject: "add b"}},
	})
	if strings.Count(p, "## Spec") != 1 || !strings.Contains(p, "spec of a") || strings.Contains(p, "spec of b") {
		t.Fatalf("prompt:\n%s", p)
	}
}

func TestResolverPromptForDependencyMergeShowsBothSides(t *testing.T) {
	t.Parallel()
	task := taskspec.Spec{ID: "g/greet", Title: "Greet", Kind: "implement", Spec: "make greet say hello",
		Acceptance: []string{"greet prints hello"}}
	hunk := "<<<<<<< HEAD\n\treturn \"hi there\"\n=======\n\treturn \"hello\"\n>>>>>>> mill: g/polite\n"
	p := Resolver(ResolverInput{
		Task:      task,
		Conflicts: []ConflictFile{{Path: "greet.go", Hunks: hunk}},
		BaseCommits: []BaseCommit{
			{SHA: "1111111111112222", Subject: "mill: g/loud attempt 1", Trailers: "Mill-Task: g/loud"},
		},
		OtherCommits: []BaseCommit{
			{SHA: "0123456789abcdef", Subject: "mill: g/polite attempt 2", Trailers: "Mill-Task: g/polite\nMill-Attempt: 2"},
		},
		Merging: "g/polite (mill/g/polite)",
	})
	for _, want := range []string{
		"mill is preparing task g/greet by merging the branches of its dependencies",
		"the merge of g/polite (mill/g/polite) stopped on conflicts",
		"## Commits already in the worktree (earlier dependencies)\n- 111111111111 mill: g/loud attempt 1\n    Mill-Task: g/loud\n",
		"## Commits of g/polite (mill/g/polite)\n- 0123456789ab mill: g/polite attempt 2\n    Mill-Task: g/polite\n    Mill-Attempt: 2\n",
		"both dependencies' changes must survive",
		hunk[:len(hunk)-1], "make greet say hello", "greet prints hello",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, unwanted := range []string{"replaying task", "Commits on ", "Keep both intents: the base commits"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("merge prompt contains restack wording %q:\n%s", unwanted, p)
		}
	}
}

// restackGolden is the restack prompt for the input in TestResolverPromptForRestackUnchanged.
// Backticks are written as ~ because a raw string literal cannot contain them.
const restackGolden = `You are resolving merge conflicts in this git worktree (your current directory). mill is replaying task g/greet onto mill/g/polite and the replay stopped on conflicts.

# Task g/greet: Greet

Kind: implement

## Spec
make greet say hello

## Acceptance criteria (the task's changes must still meet each one)
- greet prints hello

## Commits on mill/g/polite that the task's changes conflict with
- 0123456789ab mill: g/polite attempt 2
    Mill-Task: g/polite
    Mill-Attempt: 2

## Conflicted files
### greet.go
~~~
<<<<<<< HEAD
	return "hi"
=======
	return "hello"
>>>>>>> x
~~~

## Run these before you finish
- ~go test ./greet~
- The tests of the packages you changed. For Go: ~go build ./... && go vet ./...~, plus ~go test~ of those packages (with ~-run~ when the change is narrow enough).

## check_cmd (mill runs it; do not run it yourself)
mill runs check_cmd (~make check~) in the gate after you finish and returns its failures to you as findings. Do not run check_cmd yourself.

## Rules
- Edit only the conflicted files listed above.
- Remove every conflict marker (<<<<<<<, =======, >>>>>>>) from each of them.
- Keep both intents: the base commits' changes and this task's changes must both survive.
- Do not run git commands that change history or the index (commit, add, rm, reset, checkout, restore --staged, rebase, merge, cherry-pick, stash): mill stages the files and continues.
- Wait only on processes you started yourself (your own background jobs). Never wait on, poll or kill machine-wide process patterns (pgrep, pkill, ps | grep): other agents share this machine.
- Write scratch files under $TMPDIR, never in this directory.
- When you are done, give your final answer as the structured output: summary, resolved (the files you resolved).
`

func TestResolverPromptForRestackUnchanged(t *testing.T) {
	t.Parallel()
	p := Resolver(ResolverInput{
		Task:        taskspec.Spec{ID: "g/greet", Title: "Greet", Kind: "implement", Spec: "make greet say hello", Acceptance: []string{"greet prints hello"}, Checks: []string{"go test ./greet"}},
		Conflicts:   []ConflictFile{{Path: "greet.go", Hunks: "<<<<<<< HEAD\n\treturn \"hi\"\n=======\n\treturn \"hello\"\n>>>>>>> x\n"}},
		BaseCommits: []BaseCommit{{SHA: "0123456789abcdef", Subject: "mill: g/polite attempt 2", Trailers: "Mill-Task: g/polite\nMill-Attempt: 2"}},
		Onto:        "mill/g/polite",
		CheckCmd:    "make check",
	})
	want := strings.ReplaceAll(restackGolden, "~", "`")
	if p != want {
		t.Fatalf("restack prompt changed.\n--- got ---\n%s\n--- want ---\n%s", p, want)
	}
}

func TestParseResolver(t *testing.T) {
	t.Parallel()
	o, err := ParseResolver(json.RawMessage(`{"summary":"kept both","resolved":["a.go"]}`))
	if err != nil || o.Summary != "kept both" || len(o.Resolved) != 1 || o.Resolved[0] != "a.go" {
		t.Fatalf("got %+v, %v", o, err)
	}
	if _, err := ParseResolver(json.RawMessage(`{"summary":" ","resolved":[]}`)); err == nil {
		t.Fatal("empty summary accepted")
	}
	if _, err := ParseResolver(json.RawMessage(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	var schema map[string]any
	if err := json.Unmarshal(ResolverSchema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
}
