package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/scheduler"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const mergeCheckPoll = time.Millisecond

// mergeCheckBuf is a progress writer safe for the scheduler's goroutines.
type mergeCheckBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *mergeCheckBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *mergeCheckBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type mergeCheckEnv struct {
	p    *pipeline.Pipeline
	s    *store.Store
	out  *mergeCheckBuf
	root string
	goal string
}

// mergeCheckSetup creates a temporary Go module repo, a goal with tasks a and b (no
// dependencies) and a pipeline on the fake runner with millisecond gate-slot polls.
func mergeCheckSetup(t *testing.T, mergeCheckCmd string, script runner.FakeScript) *mergeCheckEnv {
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
	for name, content := range map[string]string{
		"go.mod":     "module example.com/m\n\ngo 1.21\n",
		".gitignore": ".mill/runs/\n.mill/mill.db*\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.CommitAll(ctx, root, "initial"); err != nil {
		t.Fatal(err)
	}
	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pl := plan.Plan{Goal: "goal", Assumptions: []string{}}
	for _, id := range []string{"a", "b"} {
		pl.Tasks = append(pl.Tasks, taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "do " + id,
			Acceptance: []string{"works"}, Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}})
	}
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	// Tasks are scripted by their full ids.
	tasks := map[string]runner.Script{}
	for id, sc := range script.Tasks {
		tasks[store.TaskID(g.ID, id)] = sc
	}
	script.Tasks = tasks

	cfg := config.Default()
	cfg.CheckCmd = "true"
	cfg.MergeCheckCmd = mergeCheckCmd
	cfg.GateCheckBase = false
	cfg.OwnsPolicy = "off"
	cfg.WorktreeRoot = t.TempDir()
	cfg.GateTimeout = config.Duration{Duration: 2 * time.Minute}
	cfg.Runner.Kind = "fake"
	cfg.Runner.FakeScript = "unused"
	cfg.Budget.Task = budget.Limits{}
	out := &mergeCheckBuf{}
	p := &pipeline.Pipeline{Store: s, Runner: runner.NewFake(script), Host: host.NewExec(), Cfg: cfg,
		Paths: paths, Home: t.TempDir(), Out: out, GateSlotPoll: mergeCheckPoll}
	return &mergeCheckEnv{p: p, s: s, out: out, root: root, goal: g.ID}
}

func (e *mergeCheckEnv) id(short string) string { return store.TaskID(e.goal, short) }

func (e *mergeCheckEnv) run(t *testing.T, short string) pipeline.Outcome {
	t.Helper()
	out, err := e.p.Run(context.Background(), e.id(short))
	if err != nil {
		t.Fatalf("Run %s: %v\nlog:\n%s", short, err, e.out)
	}
	return out
}

func (e *mergeCheckEnv) status(t *testing.T, short string) store.Task {
	t.Helper()
	tk, err := e.s.GetTask(context.Background(), e.id(short))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// events returns the data of the task's events of kind.
func (e *mergeCheckEnv) events(t *testing.T, short, kind string) []json.RawMessage {
	t.Helper()
	evs, err := e.s.TaskEvents(context.Background(), e.id(short))
	if err != nil {
		t.Fatal(err)
	}
	var out []json.RawMessage
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev.Data)
		}
	}
	return out
}

// assertNoMergeCheckWorktree fails when a throwaway worktree is left on disk or in git.
func (e *mergeCheckEnv) assertNoMergeCheckWorktree(t *testing.T) {
	t.Helper()
	list, err := gitx.Run(context.Background(), e.root, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, ".merge-check") {
		t.Errorf("a merge check worktree is still registered:\n%s", list)
	}
	parent := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), ".merge-check")
	entries, err := os.ReadDir(parent)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("merge check worktree directories left in %s: %v", parent, entries)
	}
}

func mcImpl(files map[string]string) runner.Step {
	return runner.Step{Output: json.RawMessage(`{"summary":"wrote files","files_changed":[],"notes":""}`), Files: files}
}

func mcPass() runner.Step {
	return runner.Step{Output: json.RawMessage(`{"verdict":"pass","summary":"ok","findings":[]}`)}
}

func mcTask(files map[string]string) runner.Script {
	return runner.Script{runner.RoleImplementer: {mcImpl(files)}, runner.RoleVerifier: {mcPass()}}
}

const hookRunFile = "package pkg\n\ntype hookRun struct{}\n"

func TestMergeCheckSendsLaterTaskBackOnDuplicateGoType(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	e := mergeCheckSetup(t, "go vet ./...", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/a.go": hookRunFile}),
		"b": mcTask(map[string]string{"pkg/b.go": hookRunFile}),
	}})
	if out := e.run(t, "a"); out.Status != store.TaskVerified {
		t.Fatalf("a = %+v, want verified\n%s", out, e.out)
	}
	out := e.run(t, "b")
	if out.Status != pipeline.OutcomeReopened {
		t.Fatalf("b outcome = %+v, want %s\n%s", out, pipeline.OutcomeReopened, e.out)
	}
	if got := e.status(t, "a").Status; got != store.TaskVerified {
		t.Errorf("a = %s, want verified", got)
	}
	b := e.status(t, "b")
	if b.Status != store.TaskPending || !strings.Contains(b.Reason, e.id("a")) {
		t.Fatalf("b = %s (%q), want pending naming %s", b.Status, b.Reason, e.id("a"))
	}
	findings, err := pipeline.LastFindings(context.Background(), e.s, e.id("b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want 1", findings)
	}
	f := findings[0]
	if f.Source != "merge-check" || !strings.Contains(f.Issue, e.id("a")) || !strings.Contains(f.Issue, "`go vet ./...`") ||
		!strings.Contains(f.Issue, "hookRun") {
		t.Errorf("finding = %+v, want source merge-check naming %s, the command and the output tail", f, e.id("a"))
	}
	evs := e.events(t, "b", pipeline.MergeCheckFailedEvent)
	if len(evs) != 1 || !strings.Contains(string(evs[0]), e.id("a")) || !strings.Contains(string(evs[0]), "go vet ./...") {
		t.Errorf("merge-check-failed events = %s, want one naming %s and the command", evs, e.id("a"))
	}
	if len(e.events(t, "a", pipeline.MergeCheckFailedEvent)) != 0 {
		t.Error("a, verified first with no verified sibling, has a merge-check-failed event")
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckReportsMergeConflict(t *testing.T) {
	t.Parallel()
	e := mergeCheckSetup(t, "true", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/x.go": "package pkg\n\nconst X = 1\n"}),
		"b": mcTask(map[string]string{"pkg/x.go": "package pkg\n\nconst X = 2\n"}),
	}})
	e.run(t, "a")
	if out := e.run(t, "b"); out.Status != pipeline.OutcomeReopened {
		t.Fatalf("b outcome = %+v, want %s\n%s", out, pipeline.OutcomeReopened, e.out)
	}
	if got := e.status(t, "b").Status; got != store.TaskPending {
		t.Fatalf("b = %s, want pending", got)
	}
	findings, err := pipeline.LastFindings(context.Background(), e.s, e.id("b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Source != "merge-check" || !strings.Contains(findings[0].Issue, e.id("a")) ||
		!strings.Contains(findings[0].Issue, "conflicts in pkg/x.go") {
		t.Errorf("findings = %+v, want one merge-check finding naming %s and pkg/x.go", findings, e.id("a"))
	}
	if len(e.events(t, "b", pipeline.MergeCheckFailedEvent)) != 1 {
		t.Error("no merge-check-failed event for b")
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckNonClashingSiblingsStayVerified(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	e := mergeCheckSetup(t, "go vet ./...", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/a.go": "package pkg\n\ntype hookA struct{}\n"}),
		"b": mcTask(map[string]string{"pkg/b.go": "package pkg\n\ntype hookB struct{}\n"}),
	}})
	e.run(t, "a")
	if out := e.run(t, "b"); out.Status != store.TaskVerified {
		t.Fatalf("b outcome = %+v, want verified\n%s", out, e.out)
	}
	for _, id := range []string{"a", "b"} {
		if got := e.status(t, id).Status; got != store.TaskVerified {
			t.Errorf("%s = %s, want verified", id, got)
		}
	}
	if !strings.Contains(e.out.String(), e.id("b")+" a1 merge check ok") {
		t.Errorf("the merge check did not run on b:\n%s", e.out)
	}
	if len(e.events(t, "b", pipeline.MergeCheckFailedEvent)) != 0 {
		t.Error("b has a merge-check-failed event")
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckEmptyCommandSkipsCheck(t *testing.T) {
	t.Parallel()
	e := mergeCheckSetup(t, "", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/a.go": hookRunFile}),
		"b": mcTask(map[string]string{"pkg/b.go": hookRunFile}),
	}})
	e.run(t, "a")
	if out := e.run(t, "b"); out.Status != store.TaskVerified {
		t.Fatalf("b outcome = %+v, want verified\n%s", out, e.out)
	}
	if strings.Contains(e.out.String(), "merge check") {
		t.Errorf("a merge check ran with merge_check_cmd empty:\n%s", e.out)
	}
	if len(e.events(t, "b", pipeline.MergeCheckFailedEvent)) != 0 {
		t.Error("b has a merge-check-failed event")
	}
}

func TestMergeCheckIgnoresSiblingsInOtherDirectories(t *testing.T) {
	t.Parallel()
	// The command fails whenever a's file is merged in.
	e := mergeCheckSetup(t, "test ! -e other/a.go", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"other/a.go": hookRunFile}),
		"b": mcTask(map[string]string{"pkg/b.go": hookRunFile}),
	}})
	e.run(t, "a")
	if out := e.run(t, "b"); out.Status != store.TaskVerified {
		t.Fatalf("b outcome = %+v, want verified\n%s", out, e.out)
	}
	if strings.Contains(e.out.String(), "merge check") {
		t.Errorf("b was merge-checked with a sibling in another directory:\n%s", e.out)
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckOnlyMergesSiblingsSharingADirectory(t *testing.T) {
	t.Parallel()
	// Three tasks would need another setup; instead a shares pkg/ with b and also touches
	// other/, and the command passes only when b's merge has pkg/a.go: a is merged.
	e := mergeCheckSetup(t, "test -e pkg/a.go && test -e pkg/b.go", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/a.go": "package pkg\n", "other/o.go": "package other\n"}),
		"b": mcTask(map[string]string{"pkg/b.go": "package pkg\n"}),
	}})
	e.run(t, "a")
	if out := e.run(t, "b"); out.Status != store.TaskVerified {
		t.Fatalf("b outcome = %+v, want verified\n%s", out, e.out)
	}
	if !strings.Contains(e.out.String(), "merge check with "+e.id("a")) {
		t.Errorf("b was not merge-checked with a, which shares pkg/:\n%s", e.out)
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckRunsInGateSlot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	started, goOn := filepath.Join(dir, "started"), filepath.Join(dir, "go")
	cmd := "touch " + started + "; while [ ! -e " + goOn + " ]; do sleep 0.01; done"
	e := mergeCheckSetup(t, cmd, runner.FakeScript{Tasks: map[string]runner.Script{
		"a": mcTask(map[string]string{"pkg/a.go": "package pkg\n"}),
		"b": mcTask(map[string]string{"pkg/b.go": "package pkg\n"}),
	}})
	e.run(t, "a")
	done := make(chan pipeline.Outcome, 1)
	go func() {
		out, err := e.p.Run(context.Background(), e.id("b"))
		if err != nil {
			t.Errorf("Run b: %v", err)
		}
		done <- out
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the merge check command never started\n%s", e.out)
		}
		time.Sleep(mergeCheckPoll)
	}
	// While the command runs, the only gate slot is held: taking one has to wait.
	waited := make(chan struct{})
	var once sync.Once
	acquired := make(chan error, 1)
	go func() {
		release, err := e.s.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "probe", Attempt: 1, PID: os.Getpid()},
			1, mergeCheckPoll, func() { once.Do(func() { close(waited) }) })
		if err == nil {
			err = release()
		}
		acquired <- err
	}()
	select {
	case <-waited:
	case err := <-acquired:
		t.Fatalf("a gate slot was free while the merge check command ran (err %v)", err)
	}
	if err := os.WriteFile(goOn, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if out := <-done; out.Status != store.TaskVerified {
		t.Fatalf("b outcome = %+v, want verified\n%s", out, e.out)
	}
	e.assertNoMergeCheckWorktree(t)
}

func TestMergeCheckReopenedTaskIsRerunBySchedulerAndEndsVerified(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	// Whichever task verifies second clashes with the other and is sent back; its second
	// attempt renames its type.
	script := func(file, renamed string) runner.Script {
		return runner.Script{
			runner.RoleImplementer: {mcImpl(map[string]string{file: hookRunFile}), mcImpl(map[string]string{file: "package pkg\n\ntype " + renamed + " struct{}\n"})},
			runner.RoleVerifier:    {mcPass(), mcPass()},
		}
	}
	e := mergeCheckSetup(t, "go vet ./...", runner.FakeScript{Tasks: map[string]runner.Script{
		"a": script("pkg/a.go", "hookRunA"),
		"b": script("pkg/b.go", "hookRunB"),
	}})
	sleep := func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(mergeCheckPoll)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	sch := &scheduler.Scheduler{Store: e.s, MaxParallel: 1, RunTask: e.p.Run, Out: e.out, Sleep: sleep}
	res, err := sch.RunGoal(context.Background(), e.goal, scheduler.Options{})
	log := e.out.String()
	if err != nil || res.Status != store.GoalDone {
		t.Fatalf("RunGoal = %+v, %v\n%s", res, err, log)
	}
	reopened := 0
	for _, id := range []string{"a", "b"} {
		if got := e.status(t, id).Status; got != store.TaskVerified {
			t.Errorf("%s = %s, want verified\n%s", id, got, log)
		}
		as, err := e.s.Attempts(context.Background(), e.id(id))
		if err != nil {
			t.Fatal(err)
		}
		impls := 0
		for _, a := range as {
			if a.Role == string(runner.RoleImplementer) {
				impls++
			}
		}
		if n := len(e.events(t, id, pipeline.MergeCheckFailedEvent)); n > 0 {
			reopened++
			if impls != 2 {
				t.Errorf("reopened task %s ran %d implementer attempt(s), want 2\n%s", id, impls, log)
			}
		}
	}
	if reopened != 1 {
		t.Errorf("%d task(s) were sent back by the merge check, want 1\n%s", reopened, log)
	}
	if strings.Contains(log, "stuck") {
		t.Errorf("the reopened task was reported stuck:\n%s", log)
	}
	e.assertNoMergeCheckWorktree(t)
}
