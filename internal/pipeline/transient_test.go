package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

const transientErr = "API Error: 529 overloaded"

func transientStep() runner.Step {
	return runner.Step{Error: transientErr, Transient: true}
}

// attemptStatuses lists "role:status" for each attempt row of the task, in order.
func (e *env) attemptStatuses(t *testing.T, id string) []string {
	t.Helper()
	as, err := e.p.Store.Attempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range as {
		got = append(got, a.Role+":"+a.Status)
	}
	return got
}

// findingsMention reports whether any recorded attempt finding mentions s.
func (e *env) findingsMention(t *testing.T, id, s string) bool {
	t.Helper()
	for _, ev := range e.events(t, id, "attempt-findings") {
		b, err := json.Marshal(ev["findings"])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}

func TestTransientImplementerErrorIsRetriedWithoutFinding(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {transientStep(), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "timp")
	out := e.run(t, "timp")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if e.findingsMention(t, "timp", "overloaded") {
		t.Fatalf("a finding mentions the transient error: %v", e.events(t, "timp", "attempt-findings"))
	}
	if ip := prompts(e.fake, runner.RoleImplementer); strings.Contains(ip[1], "overloaded") {
		t.Fatalf("the transient error was passed to the next attempt:\n%s", ip[1])
	}
	if got := e.attemptStatuses(t, "timp"); len(got) == 0 || got[0] != "implementer:transient" {
		t.Fatalf("attempts = %v, want the first implementer:transient", got)
	}
	if !strings.Contains(e.out.String(), "transient") || !strings.Contains(e.out.String(), transientErr) {
		t.Fatalf("log does not show the transient error:\n%s", e.out)
	}
}

func TestTransientVerifierErrorIsRetriedThenPasses(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {transientStep(), verdict("pass")},
	}, nil)
	e.task(t, "tver")
	out := e.run(t, "tver")
	if out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if e.findingsMention(t, "tver", "overloaded") {
		t.Fatalf("a finding mentions the transient error: %v", e.events(t, "tver", "attempt-findings"))
	}
	got := e.attemptStatuses(t, "tver")
	want := []string{"implementer:ok", "verifier:transient", "verifier:ok"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("attempts = %v, want %v", got, want)
	}
}

func TestTransientErrorOnEveryAttemptBlocksWithRetryHint(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {transientStep(), transientStep()},
	}, func(c *config.Config) { c.MaxAttempts = 2 })
	e.task(t, "tall")
	out := e.run(t, "tall")
	if out.Status != store.TaskBlocked || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if !strings.Contains(out.Reason, "transient agent errors") || !strings.Contains(out.Reason, "mill run --retry-blocked") {
		t.Fatalf("reason = %q", out.Reason)
	}
	task, err := e.p.Store.GetTask(context.Background(), "tall")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskBlocked {
		t.Fatalf("status = %s", task.Status)
	}
	if e.findingsMention(t, "tall", "overloaded") {
		t.Fatal("a finding mentions the transient error")
	}
	for _, s := range e.attemptStatuses(t, "tall") {
		if s != "implementer:transient" {
			t.Fatalf("attempts = %v", e.attemptStatuses(t, "tall"))
		}
	}
}
