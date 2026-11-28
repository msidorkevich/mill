package prompt

import (
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/taskspec"
)

const requestsHeading = "## Requests made after planning (part of this task's scope)\n"

var requestsTask = taskspec.Spec{
	ID:         "g1/a",
	Title:      "A",
	Kind:       "implement",
	Spec:       "Do A.",
	Acceptance: []string{"A is done"},
	Owns:       []string{"a.go"},
}

var testRequests = []Finding{
	{Source: "operator", Issue: "also rename the flag"},
	{Source: "ci", File: "b.go", Issue: "lint fails"},
}

const requestsList = "- [operator] also rename the flag\n- [ci] b.go: lint fails\n"

// assertOrder checks that every part occurs in s, in the given order.
func assertOrder(t *testing.T, s string, parts ...string) {
	t.Helper()
	rest := s
	for _, p := range parts {
		i := strings.Index(rest, p)
		if i < 0 {
			t.Fatalf("%q is missing or out of order in:\n%s", p, s)
		}
		rest = rest[i+len(p):]
	}
}

func TestVerifierListsRequestsAfterPlanning(t *testing.T) {
	got := Verifier(VerifierInput{
		Task:        requestsTask,
		Requests:    testRequests,
		Siblings:    []SiblingTask{{ID: "g1/b", Title: "B"}},
		OutsideOwns: []string{"b.go"},
	})
	assertOrder(t, got,
		"## Acceptance criteria",
		requestsHeading+requestsList,
		"must address every one",
		"in scope, even when they go beyond the spec or touch files outside the files the task owns",
		"Fail the verdict with a finding for any request that is not addressed",
		"Do not call changes made for a request out of scope",
		"## Other tasks of this goal",
		"## Files changed outside the files the task owns",
		"files named in a request are allowed",
	)
}

func TestImplementerListsRequestsAfterPlanning(t *testing.T) {
	got := Implementer(ImplementerInput{Task: requestsTask, Requests: testRequests, Attempt: 2, MaxAttempts: 3})
	assertOrder(t, got,
		"## Acceptance criteria",
		"## Files this task owns",
		requestsHeading+requestsList,
		"must address every one",
		"in scope, even when they go beyond the spec or touch files outside the files the task owns",
		"Keep the changes made for these requests and never revert them",
		"## Run these before you finish",
	)
}

func TestPromptsWithoutRequestsHaveNoRequestsSection(t *testing.T) {
	prompts := map[string]string{
		"implementer": Implementer(ImplementerInput{Task: requestsTask, Requests: []Finding{}}),
		"verifier":    Verifier(VerifierInput{Task: requestsTask, Requests: []Finding{}, OutsideOwns: []string{"b.go"}}),
	}
	for name, got := range prompts {
		if strings.Contains(got, "Requests made after planning") || strings.Contains(got, "request") {
			t.Errorf("%s prompt mentions requests without any:\n%s", name, got)
		}
	}
	want := "Pass only if the spec or the acceptance criteria require each of them; otherwise fail with a finding naming the file."
	if !strings.Contains(prompts["verifier"], want) {
		t.Errorf("verifier outside-owns text changed:\n%s", prompts["verifier"])
	}
}

func TestImplementerDoesNotRepeatRequestAsFinding(t *testing.T) {
	const findingsHeading = "## Findings from the previous attempt"
	other := Finding{Source: "gate", Issue: "tests fail"}
	got := Implementer(ImplementerInput{
		Task:     requestsTask,
		Requests: testRequests,
		Findings: []Finding{testRequests[0], other, testRequests[1]},
	})
	assertOrder(t, got, requestsHeading, findingsHeading, "- [gate] tests fail\n")
	for _, f := range testRequests {
		if n := strings.Count(got, f.String()); n != 1 {
			t.Errorf("%q appears %d times, want 1:\n%s", f, n, got)
		}
	}

	got = Implementer(ImplementerInput{Task: requestsTask, Requests: testRequests, Findings: testRequests})
	if strings.Contains(got, findingsHeading) {
		t.Errorf("empty findings section written:\n%s", got)
	}
}
