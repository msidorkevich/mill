package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// fileVerdict is a verifier step whose findings are about the given file → issue pairs.
func fileVerdict(v string, findings ...[2]string) runner.Step {
	fs := []prompt.Finding{}
	for _, f := range findings {
		fs = append(fs, prompt.Finding{File: f[0], Line: 1, Issue: f[1]})
	}
	b, _ := json.Marshal(map[string]any{"verdict": v, "summary": "review of the change", "findings": fs})
	return runner.Step{Output: b}
}

type outOfScopeEvent struct {
	Attempt  int                 `json:"attempt"`
	Findings []prompt.Finding    `json:"findings"`
	Owners   map[string][]string `json:"owners"`
}

func (e *env) outOfScopeEvents(t *testing.T, id string) []outOfScopeEvent {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []outOfScopeEvent
	for _, ev := range evs {
		if ev.Kind != "verifier-out-of-scope" {
			continue
		}
		var d outOfScopeEvent
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestOutOfScopeOnlyFindingVerifiesWithNotes(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a"})},
		runner.RoleVerifier:    {fileVerdict("fail", [2]string{"b.txt", "b.txt lacks a header"})},
	}, nil)
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if n := len(prompts(e.fake, runner.RoleImplementer)); n != 1 {
		t.Fatalf("implementer ran %d times, want 1", n)
	}
	task, err := e.p.Store.GetTask(context.Background(), "g1/a")
	if err != nil || task.Status != store.TaskVerified {
		t.Fatalf("task = %+v, %v", task, err)
	}
	evs := e.outOfScopeEvents(t, "g1/a")
	if len(evs) != 1 || evs[0].Attempt != 1 || len(evs[0].Findings) != 1 || evs[0].Findings[0].File != "b.txt" ||
		strings.Join(evs[0].Owners["b.txt"], ",") != "g1/b" {
		t.Fatalf("events = %+v", evs)
	}
	if !strings.Contains(e.out.String(), "g1/a a1 verdict   pass with notes: 1 out-of-scope finding(s)") {
		t.Fatalf("log lacks pass with notes:\n%s", e.out)
	}
	if strings.Contains(e.out.String(), "verdict   fail") {
		t.Fatalf("log reports a fail:\n%s", e.out)
	}
}

func TestOutOfScopeMixedFindingsOnlyInScopeReachNextAttempt(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a"}), impl(map[string]string{"a.txt": "a2"})},
		runner.RoleVerifier: {
			fileVerdict("fail", [2]string{"a.txt", "a.txt is wrong"}, [2]string{"b.txt", "b.txt lacks a header"}, [2]string{"stray.txt", "stray.txt is odd"}),
			verdict("pass"),
		},
	}, nil)
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ps := prompts(e.fake, runner.RoleImplementer)
	if len(ps) != 2 {
		t.Fatalf("implementer prompts = %d", len(ps))
	}
	if !strings.Contains(ps[1], "a.txt is wrong") || !strings.Contains(ps[1], "stray.txt is odd") || strings.Contains(ps[1], "b.txt lacks a header") {
		t.Fatalf("second prompt:\n%s", ps[1])
	}
	evs := e.outOfScopeEvents(t, "g1/a")
	if len(evs) != 1 || len(evs[0].Findings) != 1 || evs[0].Findings[0].Issue != "b.txt lacks a header" {
		t.Fatalf("events = %+v", evs)
	}
	if !strings.Contains(e.out.String(), "g1/a a1 verdict   fail: 2 finding(s)") {
		t.Fatalf("log:\n%s", e.out)
	}
}
