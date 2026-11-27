package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

func TestRecordCIFailureFindingsReachNextImplementerPrompt(t *testing.T) {
	t.Parallel()
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}), impl(map[string]string{"hello.txt": "hello again\n"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.task(t, "keep")
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	checks := []CICheck{
		{Name: "lint", State: "success", URL: "https://ci.example/lint"},
		{Name: "unit", State: "failure", URL: "https://ci.example/unit", Summary: "FAIL TestHello"},
		{Name: "e2e", State: "failure", URL: "https://ci.example/e2e", Summary: "timeout in login"},
	}
	if err := e.p.RecordCIFailure(ctx, "keep", checks); err != nil {
		t.Fatal(err)
	}
	evs := e.events(t, "keep", "attempt-findings")
	if len(evs) != 1 || evs[0]["attempt"] != float64(2) {
		t.Fatalf("attempt-findings events = %+v", evs)
	}
	if out := e.run(t, "keep"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if len(ip) != 2 {
		t.Fatalf("implementer prompts = %d, want 2", len(ip))
	}
	for _, want := range []string{"[ci] CI check `unit` failed", "https://ci.example/unit", "FAIL TestHello",
		"[ci] CI check `e2e` failed", "https://ci.example/e2e", "timeout in login"} {
		if !strings.Contains(ip[1], want) {
			t.Fatalf("implementer prompt after the CI failure lacks %q:\n%s", want, ip[1])
		}
	}
	if strings.Contains(ip[1], "lint") {
		t.Fatalf("implementer prompt mentions the passing check:\n%s", ip[1])
	}
}

func TestRecordCIFailureWithoutFailingCheckFails(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{}, nil)
	e.task(t, "keep")
	ctx := context.Background()
	err := e.p.RecordCIFailure(ctx, "keep", []CICheck{{Name: "lint", State: "success"}})
	if err == nil || !strings.Contains(err.Error(), "no failing check") {
		t.Fatalf("RecordCIFailure = %v, want an error about no failing check", err)
	}
	if evs := e.events(t, "keep", "attempt-findings"); len(evs) != 0 {
		t.Fatalf("attempt-findings events = %+v, want none", evs)
	}
}
