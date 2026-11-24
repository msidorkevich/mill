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
