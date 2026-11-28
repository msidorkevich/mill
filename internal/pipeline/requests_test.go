package pipeline

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

const requestsHeading = "## Requests made after planning (part of this task's scope)"

// requestsVerified runs id to verified on its first attempt and fails the test otherwise.
func requestsVerified(t *testing.T, e *env, id string) {
	t.Helper()
	if out := e.run(t, id); out.Status != store.TaskVerified {
		t.Fatalf("first run of %s = %+v\n%s", id, out, e.out)
	}
}

// requestsSendBack records findings against id's attempt n and puts the task back to pending.
func requestsSendBack(t *testing.T, e *env, id string, n int, findings ...prompt.Finding) {
	t.Helper()
	if err := e.p.Store.RequestChanges(context.Background(), id, n, findings, "changes requested"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorRequestReachesVerifierOnEveryLaterAttempt(t *testing.T) {
	t.Parallel()
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {
			impl(map[string]string{"hello.txt": "hello\n"}),
			impl(map[string]string{"hello.txt": "hello world\n"}),
			impl(map[string]string{"hello.txt": "hello world!\n"}),
		},
		runner.RoleVerifier: {verdict("pass"), verdict("fail", "missing the exclamation mark"), verdict("pass")},
	}, nil)
	e.task(t, "asked")
	requestsVerified(t, e, "asked")
	requestsSendBack(t, e, "asked", 1, prompt.Finding{Source: "operator", Issue: "say hello world"})
	if out := e.run(t, "asked"); out.Status != store.TaskVerified {
		t.Fatalf("second run = %+v\n%s", out, e.out)
	}
	ip, vp := prompts(e.fake, runner.RoleImplementer), prompts(e.fake, runner.RoleVerifier)
	if len(ip) != 3 || len(vp) != 3 {
		t.Fatalf("implementer prompts = %d, verifier prompts = %d, want 3 each", len(ip), len(vp))
	}
	for role, ps := range map[string][]string{"implementer": ip, "verifier": vp} {
		if strings.Contains(ps[0], requestsHeading) {
			t.Fatalf("first %s prompt has a requests section:\n%s", role, ps[0])
		}
		for i := 1; i <= 2; i++ {
			at := strings.Index(ps[i], requestsHeading)
			if at < 0 || !strings.Contains(ps[i][at:], "[operator] say hello world") {
				t.Fatalf("%s prompt %d lacks the request section:\n%s", role, i+1, ps[i])
			}
		}
	}
	if !strings.Contains(ip[2], "missing the exclamation mark") {
		t.Fatalf("third implementer prompt lacks the verifier finding:\n%s", ip[2])
	}
}

func TestTaskWithoutRequestsHasNoRequestsSection(t *testing.T) {
	t.Parallel()
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hi\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "plain")
	if out := e.run(t, "plain"); out.Status != store.TaskVerified {
		t.Fatalf("run = %+v\n%s", out, e.out)
	}
	ps := append(prompts(e.fake, runner.RoleImplementer), prompts(e.fake, runner.RoleVerifier)...)
	if len(ps) != 3 {
		t.Fatalf("prompts = %d, want 3", len(ps))
	}
	for _, p := range ps {
		if strings.Contains(p, "Requests made after planning") {
			t.Fatalf("prompt has a requests section:\n%s", p)
		}
	}
}

func TestRequestsBeforeLandingAreDropped(t *testing.T) {
	t.Parallel()
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}), impl(map[string]string{"hello.txt": "hello there\n"})},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	ctx := context.Background()
	e.task(t, "relanded")
	requestsSendBack(t, e, "relanded", 0, prompt.Finding{Source: "ci", Issue: "old lint failure"})
	if _, err := e.p.Store.AppendEvent(ctx, "task-landed", "", "relanded", map[string]any{"commit": "abc"}); err != nil {
		t.Fatal(err)
	}
	requestsVerified(t, e, "relanded")
	requestsSendBack(t, e, "relanded", 1, prompt.Finding{Source: "operator", Issue: "say hello there"})
	if out := e.run(t, "relanded"); out.Status != store.TaskVerified {
		t.Fatalf("second run = %+v\n%s", out, e.out)
	}
	ip, vp := prompts(e.fake, runner.RoleImplementer), prompts(e.fake, runner.RoleVerifier)
	if len(ip) != 2 || len(vp) != 2 {
		t.Fatalf("implementer prompts = %d, verifier prompts = %d, want 2 each", len(ip), len(vp))
	}
	if strings.Contains(ip[0], requestsHeading) || strings.Contains(vp[0], requestsHeading) {
		t.Fatalf("first attempt got the request recorded before landing:\n%s\n%s", ip[0], vp[0])
	}
	for _, p := range []string{ip[1], vp[1]} {
		if !strings.Contains(p, "[operator] say hello there") || strings.Contains(p, "old lint failure") {
			t.Fatalf("second attempt prompt:\n%s", p)
		}
	}
}

func TestScopeRequestsCollectsMergeCheckAndCIFindings(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", nil, nil)
	ctx := context.Background()
	e.task(t, "collected")
	mc := prompt.Finding{Source: "merge-check", File: "a.go", Line: 3, Issue: "conflicts with main"}
	ci := prompt.Finding{Source: "ci", Issue: "lint fails"}
	op := prompt.Finding{Source: "operator", Issue: "rename the flag"}
	for n, fs := range [][]prompt.Finding{
		{mc, {Source: "gate", Issue: "make check failed"}},
		{{Source: "verifier", File: "b.go", Issue: "wrong"}, ci, mc},
		{op, ci},
	} {
		if err := e.p.recordFindings(ctx, "collected", n+1, fs); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScopeRequests(ctx, e.p.Store, "collected")
	if err != nil {
		t.Fatal(err)
	}
	if want := []prompt.Finding{mc, ci, op}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ScopeRequests = %+v, want %+v", got, want)
	}
	none, err := ScopeRequests(ctx, e.p.Store, "unknown")
	if err != nil || len(none) != 0 {
		t.Fatalf("ScopeRequests of a task without events = %+v, %v", none, err)
	}
}
