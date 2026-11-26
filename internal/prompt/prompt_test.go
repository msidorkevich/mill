package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/taskspec"
)

var task = taskspec.Spec{
	ID: "add-reverse", Title: "Add Reverse", Kind: "implement",
	Spec:       "Add func Reverse(s string) string.",
	Acceptance: []string{"Reverse(\"abc\") == \"cba\"", "handles UTF-8"},
	Owns:       []string{"reverse.go"},
	Size:       "S",
}

func TestSchemasAreValidJSON(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]json.RawMessage{"implementer": ImplementerSchema, "verifier": VerifierSchema} {
		var v map[string]any
		if err := json.Unmarshal(s, &v); err != nil {
			t.Errorf("%s schema: %v", name, err)
		}
	}
}

func TestImplementerPrompt(t *testing.T) {
	t.Parallel()
	withCheck := task
	withCheck.Checks = []string{"go test ./..."}
	p := Implementer(ImplementerInput{Task: withCheck, Attempt: 1, MaxAttempts: 3})
	for _, want := range []string{"Attempt 1 of 3", "Add func Reverse", "handles UTF-8", "reverse.go", "`go test ./...`", "mill commits"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "Findings from the previous attempt") {
		t.Error("first attempt mentions findings")
	}

	p = Implementer(ImplementerInput{Task: task, Attempt: 2, MaxAttempts: 3, Findings: []Finding{
		{Source: "gate", Issue: "`go test` failed: FAIL TestReverse"},
		{Source: "verifier", File: "reverse.go", Line: 7, Issue: "breaks multi-byte runes"},
	}})
	for _, want := range []string{"Findings from the previous attempt", "[gate] `go test` failed", "[verifier] reverse.go:7: breaks multi-byte runes"} {
		if !strings.Contains(p, want) {
			t.Errorf("retry prompt missing %q", want)
		}
	}
}

func TestVerifierPrompt(t *testing.T) {
	t.Parallel()
	g := gate.Result{Passed: true, Commands: []gate.CmdResult{{Cmd: "go test ./...", Exit: 0}}}
	p := Verifier(VerifierInput{Task: task, BaseSHA: "0123456789abcdef0123", Diff: "+func Reverse", DiffStat: "reverse.go | 1 +", Gate: g})
	for _, want := range []string{"independent reviewer", "1. Reverse", "2. handles UTF-8", "`go test ./...`: passed", "0123456789ab", "+func Reverse"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	big := strings.Repeat("x", MaxDiffBytes+1)
	p = Verifier(VerifierInput{Task: task, Diff: big, DiffStat: "huge.go | 9999 +", Gate: g})
	if strings.Contains(p, big) || !strings.Contains(p, "huge.go | 9999 +") {
		t.Error("large diff not replaced by the stat")
	}
}

func TestParseImplementer(t *testing.T) {
	t.Parallel()
	if _, err := ParseImplementer([]byte(`{"summary":"did it","files_changed":["a.go"],"notes":""}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseImplementer([]byte(`{"summary":" "}`)); err == nil {
		t.Fatal("empty summary accepted")
	}
	if _, err := ParseImplementer([]byte(`nope`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestParseVerifier(t *testing.T) {
	t.Parallel()
	o, err := ParseVerifier([]byte(`{"verdict":"fail","summary":"s","findings":[{"file":"a.go","line":3,"issue":"bug"}]}`))
	if err != nil || o.Findings[0].Source != "verifier" {
		t.Fatalf("ParseVerifier = %+v, %v", o, err)
	}
	if _, err := ParseVerifier([]byte(`{"verdict":"pass","summary":"ok","findings":[]}`)); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		`{"verdict":"maybe","summary":"s","findings":[]}`,
		`{"verdict":"fail","summary":"s","findings":[]}`,
		`{"verdict":"fail","summary":"s","findings":[{"file":"","line":1,"issue":"x"}]}`,
		`{"verdict":"fail","summary":"s","findings":[{"file":"a","line":1,"issue":""}]}`,
		`[]`,
	}
	for _, b := range bad {
		if _, err := ParseVerifier([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func TestFindingString(t *testing.T) {
	t.Parallel()
	cases := map[Finding]string{
		{Source: "verifier", File: "a.go", Line: 3, Issue: "x"}: "[verifier] a.go:3: x",
		{Source: "verifier", File: "a.go", Issue: "x"}:          "[verifier] a.go: x",
		{Source: "gate", Issue: "x"}:                            "[gate] x",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("%+v = %q, want %q", f, got, want)
		}
	}
}

func TestVerifierPromptListsFilesOutsideOwns(t *testing.T) {
	t.Parallel()
	in := VerifierInput{Task: taskspec.Spec{ID: "t", Title: "T", Kind: "implement", Spec: "s"}, BaseSHA: "abc", Diff: "d"}
	if p := Verifier(in); strings.Contains(p, "outside the files the task owns") {
		t.Fatal("heading shown without outside files")
	}
	in.OutsideOwns = []string{"stray.go"}
	p := Verifier(in)
	for _, w := range []string{"outside the files the task owns", "- stray.go", "otherwise fail with a finding naming the file"} {
		if !strings.Contains(p, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, p)
		}
	}
}

func TestVerifierPromptNamesFlakyCommands(t *testing.T) {
	t.Parallel()
	in := VerifierInput{Task: taskspec.Spec{ID: "t", Title: "T", Kind: "implement", Spec: "s"}, BaseSHA: "abc", Diff: "d"}
	if p := Verifier(in); strings.Contains(p, "Flaky") {
		t.Fatal("flaky section shown without flaky commands")
	}
	in.Flaky = []string{"go test ./..."}
	p := Verifier(in)
	for _, w := range []string{"## Flaky gate commands", "- `go test ./...`", "failed once and passed on a re-run", "nondeterministic"} {
		if !strings.Contains(p, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, p)
		}
	}
}

func TestVerifierPromptListsSiblingTasks(t *testing.T) {
	t.Parallel()
	in := VerifierInput{Task: taskspec.Spec{ID: "g/a", Title: "A", Kind: "implement", Spec: "s"}, BaseSHA: "abc", Diff: "d"}
	if p := Verifier(in); strings.Contains(p, "Other tasks of this goal") || strings.Contains(p, "out of scope") {
		t.Fatalf("sibling section shown for a standalone task:\n%s", p)
	}
	in.Siblings = []SiblingTask{
		{ID: "g/b", Title: "Docs", Owns: []string{"README.md", "docs/**"}, Summary: "Document the command."},
		{ID: "g/c", Title: "Store", Owns: []string{"internal/store/x.go"}, Summary: "Persist x."},
	}
	p := Verifier(in)
	for _, w := range []string{"## Other tasks of this goal",
		"g/b", "Docs", "README.md, docs/**", "Document the command.",
		"g/c", "Store", "internal/store/x.go", "Persist x.",
		"out of scope", "do not fail this task"} {
		if !strings.Contains(p, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, p)
		}
	}
}
