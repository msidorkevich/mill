package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// stageRepo has goal g1 whose task a is running in phase, with no running attempt. The store
// clock is fixed at the returned time.
func stageRepo(t *testing.T, phase string) (string, time.Time) {
	t.Helper()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := taskspec.Spec{ID: "a", Title: "a", Kind: "implement", Spec: "do it", Acceptance: []string{"done"},
		Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}}
	if err := s.ApproveGoal(ctx, g.ID, plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: []taskspec.Spec{spec}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "g1/a", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, id, "done", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskRunning, phase, ""); err != nil {
		t.Fatal(err)
	}
	return root, at
}

func TestWatchListsGateStageWithElapsedAndGateLogLastLine(t *testing.T) {
	root, at := stageRepo(t, store.PhaseGate)
	paths := project.PathsAt(root)
	dir := filepath.Join(paths.Runs, "g1/a", "a1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gate-0.log")
	if err := os.WriteFile(logPath, []byte("compiling\nok  pkg 0.3s\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	if err := renderWatch(context.Background(), &b, paths, s, "", at.Add(95*time.Second)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"g1/a", "#1", "gate", "1m35s", logPath, "ok  pkg 0.3s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nothing running") {
		t.Errorf("claims nothing running:\n%s", out)
	}
}

func TestWatchListsOtherRunningStages(t *testing.T) {
	root, at := stageRepo(t, "commit")
	paths := project.PathsAt(root)
	s, err := store.Open(context.Background(), paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	if err := renderWatch(context.Background(), &b, paths, s, "", at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "g1/a") || !strings.Contains(out, "commit") || !strings.Contains(out, "0m05s") || strings.Contains(out, "nothing running") {
		t.Errorf("stage not listed:\n%s", out)
	}
}
