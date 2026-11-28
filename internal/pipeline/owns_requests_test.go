package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// ownsReqSetup builds goal g1 with task a owning a.txt and siblings b and c owning b.txt and
// c.txt, under the given owns policy and one attempt per run. The fake implementer first
// writes a.txt, then the files of second; the verifier steps follow the given ones.
func ownsReqSetup(t *testing.T, policy string, second map[string]string, verdicts ...runner.Step) *env {
	t.Helper()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a\n"}), impl(second)},
		runner.RoleVerifier:    append([]runner.Step{verdict("pass")}, verdicts...),
	}, func(c *config.Config) { c.OwnsPolicy = policy; c.MaxAttempts = 1 })
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"b.txt"}},
		goalTask{id: "c", owns: []string{"c.txt"}},
	)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("first run of g1/a = %+v\n%s", out, e.out)
	}
	return e
}

// ownsReqSendBack sends g1/a back with an operator request naming file.
func ownsReqSendBack(t *testing.T, e *env, file string) {
	t.Helper()
	f := prompt.Finding{Source: "operator", File: file, Issue: "also update " + file}
	if err := e.p.Store.RequestChanges(context.Background(), "g1/a", 1, []prompt.Finding{f}, "changes requested"); err != nil {
		t.Fatal(err)
	}
}

// ownsReqEvent is the data of one owns-outside event.
type ownsReqEvent struct {
	Files     []string            `json:"files"`
	Owners    map[string][]string `json:"owners"`
	Requested []string            `json:"requested"`
}

// ownsReqEvents returns the owns-outside events of task id.
func ownsReqEvents(t *testing.T, e *env, id string) []ownsReqEvent {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []ownsReqEvent
	for _, ev := range evs {
		if ev.Kind != "owns-outside" {
			continue
		}
		var d ownsReqEvent
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestOwnsCheckAllowsFileNamedInOperatorRequest(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"warn", "enforce"} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			e := ownsReqSetup(t, policy, map[string]string{"b.txt": "requested\n", "c.txt": "requested too\n"}, verdict("pass"))
			// One request naming two files, as merge-check findings do.
			ownsReqSendBack(t, e, "b.txt, c.txt")
			if out := e.run(t, "g1/a"); out.Status != store.TaskVerified || out.Attempts != 1 {
				t.Fatalf("second run = %+v\n%s", out, e.out)
			}
			if strings.Contains(e.out.String(), "owned by") {
				t.Fatalf("log names a sibling owner:\n%s", e.out)
			}
			evs := ownsReqEvents(t, e, "g1/a")
			if len(evs) != 1 || strings.Join(evs[0].Files, ",") != "b.txt,c.txt" ||
				strings.Join(evs[0].Requested, ",") != "b.txt,c.txt" || len(evs[0].Owners) != 0 {
				t.Fatalf("owns-outside events = %+v", evs)
			}
			f, err := LastFindings(context.Background(), e.p.Store, "g1/a")
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range f {
				if x.Source == "mill" {
					t.Fatalf("owns finding recorded: %+v", f)
				}
			}
		})
	}
}

func TestOwnsCheckStillRefusesUnrequestedFile(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"warn", "enforce"} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			e := ownsReqSetup(t, policy, map[string]string{"b.txt": "requested\n", "c.txt": "not requested\n", "stray.txt": "x\n"})
			ownsReqSendBack(t, e, "b.txt")
			if out := e.run(t, "g1/a"); out.Status != store.TaskBlocked {
				t.Fatalf("second run = %+v\n%s", out, e.out)
			}
			if n := len(prompts(e.fake, runner.RoleVerifier)); n != 1 {
				t.Fatalf("verifier ran %d time(s), want only the first run's", n)
			}
			f, err := LastFindings(context.Background(), e.p.Store, "g1/a")
			if err != nil {
				t.Fatal(err)
			}
			byFile := map[string]string{}
			for _, x := range f {
				if x.Source != "mill" {
					t.Fatalf("findings = %+v", f)
				}
				byFile[x.File] = x.Issue
			}
			want := 1
			if policy == "enforce" {
				want = 2
				if !strings.Contains(byFile["stray.txt"], "outside the task's owns") {
					t.Fatalf("findings = %+v", f)
				}
			}
			if len(f) != want || !strings.Contains(byFile["c.txt"], "owned by task g1/c") || byFile["b.txt"] != "" {
				t.Fatalf("findings = %+v", f)
			}
			evs := ownsReqEvents(t, e, "g1/a")
			if len(evs) != 1 || strings.Join(evs[0].Files, ",") != "b.txt,c.txt,stray.txt" ||
				strings.Join(evs[0].Requested, ",") != "b.txt" || len(evs[0].Owners) != 1 || strings.Join(evs[0].Owners["c.txt"], ",") != "g1/c" {
				t.Fatalf("owns-outside events = %+v", evs)
			}
		})
	}
}

func TestOutOfScopeSplitKeepsRequestedFile(t *testing.T) {
	t.Parallel()
	e := ownsReqSetup(t, "warn", map[string]string{"b.txt": "requested\n"}, owndVerdict("fail", "b.txt", "the requested edit to b.txt is incomplete"))
	ownsReqSendBack(t, e, "b.txt")
	if out := e.run(t, "g1/a"); out.Status == store.TaskVerified {
		t.Fatalf("second run = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	f, err := LastFindings(ctx, e.p.Store, "g1/a")
	if err != nil || len(f) != 1 || f[0].File != "b.txt" || !strings.Contains(f[0].Issue, "incomplete") {
		t.Fatalf("findings = %+v, %v", f, err)
	}
	evs, err := e.p.Store.TaskEvents(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == "verifier-out-of-scope" {
			t.Fatalf("verifier-out-of-scope event took the requested file's finding: %s", ev.Data)
		}
	}

	// A finding about a sibling's file that no request names still goes to the sibling.
	a, err := e.p.Store.GetTask(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	requests := []prompt.Finding{{Source: "merge-check", File: "a.txt, b.txt", Issue: "conflicts"}}
	findings := []prompt.Finding{{Source: "verifier", File: "b.txt", Issue: "requested"}, {Source: "verifier", File: "c.txt", Issue: "unrequested"}}
	in, out, owners, err := e.p.splitOutOfScope(ctx, a, findings, []string{"b.txt", "c.txt"}, requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 || in[0].File != "b.txt" || len(out) != 1 || out[0].File != "c.txt" || strings.Join(owners["c.txt"], ",") != "g1/c" {
		t.Fatalf("in = %+v, out = %+v, owners = %+v", in, out, owners)
	}
}
