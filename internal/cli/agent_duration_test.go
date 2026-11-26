package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// agentDurationRepo returns a repo with task "solo" holding two finished attempts recorded
// under an injected clock: an implementer whose fake run reports 1m30s of agent time but
// took 25m of wall time, and a verifier with no agent duration and 7m of wall time.
func agentDurationRepo(t *testing.T) string {
	t.Helper()
	root := planRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, mustSpec(t, taskModelsSpec)); err != nil {
		t.Fatal(err)
	}
	fake := runner.NewFake(runner.Script{
		runner.RoleImplementer: {{Output: json.RawMessage(`{}`), DurationMS: 90_000}},
		runner.RoleVerifier:    {{Output: json.RawMessage(`{}`)}},
	})
	for _, step := range []struct {
		n    int
		role runner.Role
		wall time.Duration
	}{{1, runner.RoleImplementer, 25 * time.Minute}, {1, runner.RoleVerifier, 7 * time.Minute}} {
		id, err := s.StartAttempt(ctx, "solo", step.n, string(step.role))
		if err != nil {
			t.Fatal(err)
		}
		res, err := fake.Run(ctx, runner.Request{Role: step.role, TaskID: "solo", Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(step.wall)
		if err := s.FinishAttempt(ctx, id, string(res.Status), res.Usage, false, "", "", res.Duration); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTaskShowPrintsAgentDurationOverLongerWallTime(t *testing.T) {
	t.Parallel()
	root := agentDurationRepo(t)
	r := run(t, root, nil, "task", "show", "solo")
	if r.code != ExitOK {
		t.Fatalf("task show: %+v", r)
	}
	line := attemptLine(r.stdout, "implementer")
	if !strings.HasSuffix(line, " 1m30s") {
		t.Errorf("implementer line = %q, want the agent's 1m30s\n%s", line, r.stdout)
	}
	if strings.Contains(line, "25m") {
		t.Errorf("implementer line shows wall time: %q", line)
	}
}

func TestTaskShowWithoutAgentDurationPrintsWallTime(t *testing.T) {
	t.Parallel()
	root := agentDurationRepo(t)
	r := run(t, root, nil, "task", "show", "solo")
	if r.code != ExitOK {
		t.Fatalf("task show: %+v", r)
	}
	if line := attemptLine(r.stdout, "verifier"); !strings.HasSuffix(line, " 7m0s") {
		t.Errorf("verifier line = %q, want the 7m0s wall time\n%s", line, r.stdout)
	}
}

func TestTaskShowJSONHasWallTimesAndAgentDuration(t *testing.T) {
	t.Parallel()
	root := agentDurationRepo(t)
	r := run(t, root, nil, "task", "show", "solo", "--json")
	if r.code != ExitOK {
		t.Fatalf("task show --json: %+v", r)
	}
	var doc struct {
		Attempts []map[string]json.RawMessage `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, r.stdout)
	}
	if len(doc.Attempts) != 2 {
		t.Fatalf("attempts = %s", r.stdout)
	}
	type times struct {
		StartedAt time.Time `json:"started_at"`
		EndedAt   time.Time `json:"ended_at"`
	}
	for i, want := range []struct {
		wall    time.Duration
		agentMS string
	}{{25 * time.Minute, "90000"}, {7 * time.Minute, "null"}} {
		a := doc.Attempts[i]
		for _, k := range []string{"started_at", "ended_at", "agent_ms"} {
			if _, ok := a[k]; !ok {
				t.Errorf("attempt %d has no %s: %s", i, k, r.stdout)
			}
		}
		var tm times
		b, _ := json.Marshal(a)
		if err := json.Unmarshal(b, &tm); err != nil {
			t.Fatal(err)
		}
		if got := tm.EndedAt.Sub(tm.StartedAt); got != want.wall {
			t.Errorf("attempt %d wall time = %v, want %v", i, got, want.wall)
		}
		if got := string(a["agent_ms"]); got != want.agentMS {
			t.Errorf("attempt %d agent_ms = %s, want %s", i, got, want.agentMS)
		}
	}
}

// attemptLine returns the first attempt row of role in task show output, whitespace collapsed.
func attemptLine(out, role string) string {
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == role {
			return strings.Join(f, " ")
		}
	}
	return ""
}
