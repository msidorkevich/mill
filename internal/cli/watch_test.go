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
	t.Parallel()
	root := watchRepo(t)
	r := run(t, root, nil, "watch", "--once", "--interval", groupBWatchInterval)
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
	t.Parallel()
	root := watchRepo(t)
	r := run(t, root, nil, "watch", "--interval", groupBWatchInterval)
	if r.code != ExitOK || strings.Contains(r.stdout, "\x1b[") || !strings.Contains(r.stdout, "g1/a") {
		t.Fatalf("watch: %+v", r)
	}
}

func TestWatchNarrowsToGoalOrTaskAndRejectsUnknownIds(t *testing.T) {
	t.Parallel()
	root := watchRepo(t)
	if r := run(t, root, nil, "watch", "g1", "--once", "--interval", groupBWatchInterval); r.code != ExitOK || !strings.Contains(r.stdout, "goal g1") || !strings.Contains(r.stdout, "g1/a") {
		t.Fatalf("goal: %+v", r)
	}
	if r := run(t, root, nil, "watch", "g1/a", "--once", "--interval", groupBWatchInterval); r.code != ExitOK || !strings.Contains(r.stdout, "1 running") {
		t.Fatalf("task: %+v", r)
	}
	r := run(t, root, nil, "watch", "g1/b", "--once", "--interval", groupBWatchInterval)
	if r.code != ExitOK || !strings.Contains(r.stdout, "nothing running") || !strings.Contains(r.stdout, "1 pending") {
		t.Fatalf("idle task: %+v", r)
	}
	for _, id := range []string{"g9", "g1/zzz"} {
		if r := run(t, root, nil, "watch", id, "--once", "--interval", groupBWatchInterval); r.code != ExitError || !strings.Contains(r.stderr, id) {
			t.Errorf("watch %s: %+v", id, r)
		}
	}
}

func TestWatchSaysNothingRunningWhenIdle(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	r := run(t, root, nil, "watch", "--once", "--interval", groupBWatchInterval)
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
	t.Parallel()
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
	t.Parallel()
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

// claimRepo has goal g1 whose task a is verified and claimed by a live "mill run" (pid 4242)
// that says it is merging, and whose task b is pending. The store clock is fixed at the
// returned time; the returned store treats every claim as alive.
func claimRepo(t *testing.T) (project.Paths, *store.Store, time.Time) {
	t.Helper()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	paths := project.PathsAt(root)
	s, err := store.Open(ctx, paths.DB, store.Options{
		Now:        func() time.Time { return at },
		ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := func(id string) taskspec.Spec {
		return taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "do it", Acceptance: []string{"done"},
			Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}}
	}
	if err := s.ApproveGoal(ctx, g.ID, plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: []taskspec.Spec{spec("a"), spec("b")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	c := store.Claimer{Owner: "o1", PID: 4242, Command: "run", Executable: "mill"}
	if err := s.ClaimTasks(ctx, c, []string{"g1/a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClaimActivity(ctx, c, "g1/a", "merging"); err != nil {
		t.Fatal(err)
	}
	return paths, s, at
}

func TestWatchListsClaimedVerifiedTaskWithHolderActivityAndHeldTime(t *testing.T) {
	t.Parallel()
	paths, s, at := claimRepo(t)
	var b strings.Builder
	if err := renderWatch(context.Background(), &b, paths, s, "", at.Add(95*time.Second)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	want := "g1/a  claimed by run (pid 4242): merging  held 1m35s"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "nothing running") {
		t.Errorf("says nothing running while a claim is live:\n%s", out)
	}
	if strings.Contains(out, "g1/b") {
		t.Errorf("lists unclaimed task b:\n%s", out)
	}
}

func TestWatchShowsDashForClaimWithoutActivityAndSkipsClaimsOutOfScope(t *testing.T) {
	t.Parallel()
	paths, s, at := claimRepo(t)
	ctx := context.Background()
	c := store.Claimer{Owner: "o2", PID: 77, Command: "task run"}
	if err := s.ClaimTasks(ctx, c, []string{"g1/b"}); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderWatch(ctx, &b, paths, s, "g1/b", at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if want := "g1/b  claimed by task run (pid 77): -  held 0m05s"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "g1/a") || strings.Contains(out, "nothing running") {
		t.Errorf("watch of g1/b shows g1/a or nothing running:\n%s", out)
	}
}

func TestWatchDoesNotRepeatClaimOfRunningTask(t *testing.T) {
	t.Parallel()
	paths, s, at := claimRepo(t)
	ctx := context.Background()
	// The claim on a is still held when a starts running, as mill run holds it during its attempt.
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "g1/a", 1, "implementer"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimedTasks(ctx); err != nil || got["g1/a"].Owner != "o1" {
		t.Fatalf("claim on g1/a = %+v, %v; want it still held", got, err)
	}
	var b strings.Builder
	if err := renderWatch(ctx, &b, paths, s, "", at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "g1/a") || strings.Contains(out, "claimed by") {
		t.Errorf("running task should be listed once, without its claim:\n%s", out)
	}
}
