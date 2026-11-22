package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// watchRepo has goal g1 with tasks a and b; a has a running implement attempt whose stream
// ends in a Bash call, with usage recorded on the attempt row.
func watchRepo(t *testing.T) string {
	t.Helper()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := func(id string) taskspec.Spec {
		return taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "do it", Acceptance: []string{"done"},
			Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}}
	}
	pl := plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: []taskspec.Spec{spec("a"), spec("b")}}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "g1/a", 2, "implement")
	if err != nil {
		t.Fatal(err)
	}
	stream := filepath.Join(t.TempDir(), "implement.stream.jsonl")
	body := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}` + "\n" +
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","name":"Read","input":{"fi`
	if err := os.WriteFile(stream, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, id, 1, "claude", stream); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAttemptUsage(ctx, id, budget.Usage{InputTokens: 1000, OutputTokens: 234, CostUSD: 0.5}, true); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWatchOnceListsRunningAttemptsWithLatestActionAndCounts(t *testing.T) {
	root := watchRepo(t)
	r := run(t, root, nil, "watch", "--once")
	if r.code != ExitOK {
		t.Fatalf("watch: %+v", r)
	}
	for _, want := range []string{"1 pending", "1 running", "g1/a", "#2", "implement", "1,234", "~$0.50", "Bash go test ./..."} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "nothing running") {
		t.Errorf("claims nothing running:\n%s", r.stdout)
	}
}

func TestWatchWithoutTerminalPrintsOnceAndExits(t *testing.T) {
	root := watchRepo(t)
	r := run(t, root, nil, "watch")
	if r.code != ExitOK || strings.Contains(r.stdout, "\x1b[") || !strings.Contains(r.stdout, "g1/a") {
		t.Fatalf("watch: %+v", r)
	}
}

func TestWatchNarrowsToGoalOrTaskAndRejectsUnknownIds(t *testing.T) {
	root := watchRepo(t)
	if r := run(t, root, nil, "watch", "g1", "--once"); r.code != ExitOK || !strings.Contains(r.stdout, "goal g1") || !strings.Contains(r.stdout, "g1/a") {
		t.Fatalf("goal: %+v", r)
	}
	if r := run(t, root, nil, "watch", "g1/a", "--once"); r.code != ExitOK || !strings.Contains(r.stdout, "1 running") {
		t.Fatalf("task: %+v", r)
	}
	r := run(t, root, nil, "watch", "g1/b", "--once")
	if r.code != ExitOK || !strings.Contains(r.stdout, "nothing running") || !strings.Contains(r.stdout, "1 pending") {
		t.Fatalf("idle task: %+v", r)
	}
	for _, id := range []string{"g9", "g1/zzz"} {
		if r := run(t, root, nil, "watch", id, "--once"); r.code != ExitError || !strings.Contains(r.stderr, id) {
			t.Errorf("watch %s: %+v", id, r)
		}
	}
}

func TestWatchSaysNothingRunningWhenIdle(t *testing.T) {
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	r := run(t, root, nil, "watch", "--once")
	if r.code != ExitOK || !strings.Contains(r.stdout, "nothing running") {
		t.Fatalf("watch: %+v", r)
	}
}
