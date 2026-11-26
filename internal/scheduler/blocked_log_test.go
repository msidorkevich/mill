package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// newPipeline creates a temporary git repo and a real pipeline on s that plays script with the
// fake runner and gives each task one attempt.
func newPipeline(t *testing.T, s *store.Store, script runner.FakeScript, out *syncBuf) *pipeline.Pipeline {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "initial"); err != nil {
		t.Fatal(err)
	}
	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	cfg.GateCheckBase = false
	cfg.MaxAttempts = 1
	cfg.WorktreeRoot = t.TempDir()
	cfg.Runner.Kind = "fake"
	cfg.Runner.FakeScript = "unused"
	cfg.Budget.Task = budget.Limits{}
	return &pipeline.Pipeline{Store: s, Runner: runner.NewFake(script), Host: host.NewExec(), Cfg: cfg,
		Paths: paths, Home: t.TempDir(), Out: out, GateSlotPoll: time.Millisecond}
}

// A blocked task is logged once, by the pipeline; the scheduler adds no line of its own for it,
// while a failed task still gets the scheduler's status line.
func TestBlockedTaskIsLoggedOnceByPipeline(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"}, task{id: "e"})
	a, e := store.TaskID(g, "a"), store.TaskID(g, "e")
	// The verifier rejects a's only attempt, which blocks it. e has no scripted implementer
	// step, so its run ends in a mill error, which fails it.
	script := runner.FakeScript{Tasks: map[string]runner.Script{
		a: {
			runner.RoleImplementer: {{
				Output: json.RawMessage(`{"summary":"wrote a.go","files_changed":[],"notes":""}`),
				Files:  map[string]string{"a.go": "package a\n"},
			}},
			runner.RoleVerifier: {{
				Output: json.RawMessage(`{"verdict":"fail","summary":"wrong","findings":[{"file":"a.go","line":1,"issue":"wrong"}]}`),
			}},
		},
	}}
	out := &syncBuf{}
	p := newPipeline(t, s, script, out)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: p.Run, Out: out, Sleep: pollSleep}
	res, err := sch.RunGoal(context.Background(), g, Options{})
	log := out.String()
	if err != nil || res.Status != store.GoalBlocked {
		t.Fatalf("RunGoal = %+v, %v\n%s", res, err, log)
	}

	if n := strings.Count(log, a+": blocked:"); n != 1 {
		t.Errorf("task %s has %d blocked lines, want 1:\n%s", a, n, log)
	}
	failed := status(t, s, e)
	if failed.Status != store.TaskFailed {
		t.Fatalf("task %s = %s, want failed:\n%s", e, failed.Status, log)
	}
	// The scheduler's line ends at the reason; the pipeline's goes on with the task total.
	if !strings.Contains(log, e+": failed: "+failed.Reason+"\n") {
		t.Errorf("failed task %s lacks its scheduler status line:\n%s", e, log)
	}
}

// The scheduler logs the mill error of a run that left its task blocked. The real pipeline
// fails a task on a mill error, so a stub stands in for it.
func TestBlockedTaskWithMillErrorLogsError(t *testing.T) {
	t.Parallel()
	s, g := newGoal(t, task{id: "a"})
	a := store.TaskID(g, "a")
	f := newFake(s)
	f.decide = func(string) (string, string, error) {
		return store.TaskBlocked, "gate failed", errors.New("boom")
	}
	out := &syncBuf{}
	sch := &Scheduler{Store: s, MaxParallel: 1, RunTask: f.run, Out: out, Sleep: pollSleep}
	res, err := sch.RunGoal(context.Background(), g, Options{})
	log := out.String()
	if err != nil || res.Status != store.GoalBlocked {
		t.Fatalf("RunGoal = %+v, %v\n%s", res, err, log)
	}
	if !strings.Contains(log, a+": mill error: boom\n") {
		t.Errorf("blocked task %s with a mill error lacks the error line:\n%s", a, log)
	}
	if n := strings.Count(log, a+": blocked:"); n != 1 {
		t.Errorf("task %s has %d blocked lines, want 1:\n%s", a, n, log)
	}
}
