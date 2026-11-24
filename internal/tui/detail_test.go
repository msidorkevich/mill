package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// taskShowJSON is what `mill task show <id> --json` prints for a task with n attempts.
func taskShowJSON(t *testing.T, sp taskspec.Spec, n int, cost float64) string {
	t.Helper()
	v := struct {
		Schema   string          `json:"schema"`
		Task     store.Task      `json:"task"`
		Attempts []store.Attempt `json:"attempts"`
		Usage    budget.Usage    `json:"usage"`
	}{Schema: "mill/task@1", Task: store.Task{Spec: sp, Status: store.TaskVerified, CreatedAt: clock, UpdatedAt: clock}, Usage: budget.Usage{CostUSD: cost}}
	for i := 1; i <= n; i++ {
		v.Attempts = append(v.Attempts, store.Attempt{ID: int64(i), TaskID: sp.ID, N: i, Role: "implementer", Status: "succeeded", StartedAt: clock})
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func reviewShowJSON(t *testing.T, id string) string {
	t.Helper()
	v := map[string]any{
		"schema": "mill/review@1",
		"task":   store.Task{Spec: taskspec.Spec{ID: id}, Status: store.TaskVerified},
		"verdict": prompt.VerifierOutput{Verdict: "fail", Summary: "The nil case is unhandled.",
			Findings: []prompt.Finding{{Source: "verifier", File: "a.go", Line: 12, Issue: "nil map write"}}},
		"gate": gate.Result{Passed: false, Reason: "a check failed",
			Commands: []gate.CmdResult{{Cmd: "make check", Exit: 0}, {Cmd: "go test ./a", Exit: 2}}},
		"events": []any{}, "from": "abc", "diff_stat": "a.go | 2 +-",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func diffText() string {
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "+diff line %03d\n", i)
	}
	return b.String()
}

// openDetail opens Task detail for g1/m with scripted CLI output.
func openDetail(t *testing.T, s Store) (*Model, *FakeRunner) {
	t.Helper()
	m, f := newModel(t, s)
	sp := spec("m", "z")
	sp.ID, sp.Spec, sp.Acceptance = "g1/m", "Handle the nil map.", []string{"nil input returns an error", "the table test covers it"}
	f.Script(FakeResult{Stdout: taskShowJSON(t, sp, 3, 1.25)}, "task", "show", "g1/m", "--json")
	f.Script(FakeResult{Stdout: reviewShowJSON(t, "g1/m")}, "review", "show", "g1/m", "--json")
	f.Script(FakeResult{Stdout: diffText()}, "review", "show", "g1/m", "--diff")
	toReview(m)
	press(m, "down")
	drain(m, press(m, "enter"))
	return m, f
}

// firstDiffLine returns the first diff line on the screen.
func firstDiffLine(t *testing.T, m *Model) string {
	t.Helper()
	for _, l := range strings.Split(screen(m), "\n") {
		if strings.HasPrefix(l, "+diff line") {
			return strings.TrimRight(l, " ")
		}
	}
	t.Fatalf("no diff line shown:\n%s", screen(m))
	return ""
}

func TestDetailShowsTaskReviewAndDiffFromCLI(t *testing.T) {
	t.Parallel()
	m, _ := openDetail(t, seedReview(t))
	v := screen(m)
	for _, want := range []string{
		"g1/m  verified  M",
		"attempts: 3  cost: $1.25",
		"Handle the nil map.",
		"- nil input returns an error",
		"- the table test covers it",
		"last verdict: fail: The nil case is unhandled.",
		"[verifier] a.go:12: nil map write",
		"last gate: failed: a check failed",
		"✓ make check (exit 0)",
		"✗ go test ./a (exit 2)",
		"+diff line 001",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestDetailDiffScrolls(t *testing.T) {
	t.Parallel()
	m, _ := openDetail(t, seedReview(t))
	if got := firstDiffLine(t, m); got != "+diff line 001" {
		t.Fatalf("first diff line = %q, want +diff line 001", got)
	}
	press(m, "down")
	if got := firstDiffLine(t, m); got != "+diff line 002" {
		t.Errorf("after down, first diff line = %q, want +diff line 002", got)
	}
	press(m, "pgdown")
	after := firstDiffLine(t, m)
	if after == "+diff line 002" || after == "+diff line 001" {
		t.Errorf("pgdown did not scroll: first diff line = %q", after)
	}
	press(m, "pgup")
	if got := firstDiffLine(t, m); got != "+diff line 002" {
		t.Errorf("after pgup, first diff line = %q, want +diff line 002", got)
	}
	press(m, "up")
	if got := firstDiffLine(t, m); got != "+diff line 001" {
		t.Errorf("after up, first diff line = %q, want +diff line 001", got)
	}
}

func TestDetailFailedCaptureShowsStderr(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seedReview(t))
	f.Script(FakeResult{Stderr: "mill task show: task g1/z: database is locked\n", Code: 1}, "task", "show", "g1/z", "--json")
	f.Script(FakeResult{Stderr: "mill review show: g1/z has no branch\n", Code: 1}, "review", "show", "g1/z", "--json")
	f.Script(FakeResult{Stderr: "mill review show: no diff\n", Code: 1}, "review", "show", "g1/z", "--diff")
	toReview(m)
	drain(m, press(m, "enter"))
	v := screen(m)
	for _, want := range []string{
		"✗ mill task show g1/z --json failed with exit code 1: mill task show: task g1/z: database is locked",
		"✗ mill review show g1/z --json failed with exit code 1: mill review show: g1/z has no branch",
		"✗ mill review show g1/z --diff failed with exit code 1: mill review show: no diff",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "attempts: 0") || strings.Contains(v, "last verdict: none") {
		t.Errorf("failed captures shown as empty fields:\n%s", v)
	}
}

func TestDetailRefreshesOnEnterAndAfterAnAction(t *testing.T) {
	t.Parallel()
	m, f := openDetail(t, seedReview(t))
	shows := func() int {
		n := 0
		for _, c := range f.Calls() {
			if c.Method == "Capture" && strings.Join(c.Args, " ") == "task show g1/m --json" {
				n++
			}
		}
		return n
	}
	if n := shows(); n != 1 {
		t.Fatalf("task show captured %d times on open, want 1", n)
	}
	drain(m, press(m, "enter"))
	if n := shows(); n != 2 {
		t.Errorf("task show captured %d times after enter, want 2", n)
	}
	drain(m, press(m, "a")) // the approve finishes at once
	if n := shows(); n != 3 {
		t.Errorf("task show captured %d times after the approve finished, want 3", n)
	}
}

func TestDetailLogTailsLatestAttemptActions(t *testing.T) {
	t.Parallel()
	s := seedReview(t)
	ctx := context.Background()
	dir := t.TempDir()
	old, latest := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "latest.jsonl")
	tool := func(name, input string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	if err := os.WriteFile(old, []byte(tool("Read", `{"file_path":"old.go"}`)), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := tool("Read", `{"file_path":"m.go"}`) + `{"type":"user","message":{"content":[]}}` + "\n" + tool("Bash", `{"command":"go test ./..."}`)
	if err := os.WriteFile(latest, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{old, latest} {
		id, err := s.StartAttempt(ctx, "g1/m", i+1, "implementer")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetAttemptProcess(ctx, id, 0, "claude", p); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := openDetail(t, s)
	if strings.Contains(screen(m), "Read m.go") {
		t.Fatalf("log shown before l:\n%s", screen(m))
	}
	press(m, "l")
	v := screen(m)
	for _, want := range []string{"  Read m.go\n", "  Bash go test ./...\n"} {
		if !strings.Contains(v, want) {
			t.Errorf("log lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "old.go") {
		t.Errorf("log shows an older attempt:\n%s", v)
	}

	f, err := os.OpenFile(latest, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tool("Edit", `{"file_path":"m_test.go"}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	m.Update(tickMsg(clock))
	if v := screen(m); !strings.Contains(v, "  Edit m_test.go\n") {
		t.Errorf("tick did not re-read the log:\n%s", v)
	}
	press(m, "l")
	if v := screen(m); strings.Contains(v, "Read m.go") {
		t.Errorf("second l did not hide the log:\n%s", v)
	}
}
