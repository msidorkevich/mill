package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/scheduler"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const taskFile = `{
  "id": "hello",
  "title": "Say hello",
  "kind": "implement",
  "spec": "Create hello.txt containing hello.",
  "acceptance": ["hello.txt contains hello"]
}`

// taskRepo makes an initialized repo with a commit on main, a fake runner script and a
// task file. Returns the repo root and the task file path.
func taskRepo(t *testing.T, script string, maxAttempts int) (string, string) {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	p := project.PathsAt(root)
	cfg := `check_cmd = "grep -q hello hello.txt"
max_attempts = ` + itoa(maxAttempts) + `
worktree_root = "` + t.TempDir() + `"
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	if err := os.WriteFile(p.Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(tf, []byte(taskFile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, tf
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const passScript = `{
  "implementer": [{"output": {"summary": "wrote hello", "files_changed": ["hello.txt"], "notes": ""},
                   "files": {"hello.txt": "hello\n"},
                   "usage": [{"input_tokens": 1200, "output_tokens": 300, "cost_usd": 0.05}]}],
  "verifier": [{"output": {"verdict": "pass", "summary": "looks right", "findings": []}}]
}`

func TestTaskRunAndShow(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	for _, want := range []string{"hello a1 implementer ok", "hello a1 gate      ok", "verdict   pass", "hello: verified", "Review: git -C"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	// Running the same task again needs --retry, and a verified task cannot be retried.
	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitError || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("second run: %+v", r)
	}
	if r := run(t, root, nil, "task", "run", "--file", tf, "--retry"); r.code != ExitError || !strings.Contains(r.stderr, "already verified") {
		t.Fatalf("retry of verified: %+v", r)
	}

	r = run(t, root, nil, "task", "show", "hello")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	for _, want := range []string{"status    verified", "branch    mill/t/hello", "1,500 tokens", "$0.05", "implementer", "verifier", "Last verdict: pass: looks right"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show missing %q:\n%s", want, r.stdout)
		}
	}

	r = run(t, root, nil, "task", "show", "hello", "--json")
	var v struct {
		Schema string `json:"schema"`
		Task   struct {
			Status string `json:"status"`
		} `json:"task"`
		Attempts []json.RawMessage `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.Schema != "mill/task@1" || v.Task.Status != "verified" || len(v.Attempts) != 2 {
		t.Fatalf("show --json: %v\n%s", err, r.stdout)
	}
}

func TestTaskRunBlockedThenRetry(t *testing.T) {
	script := `{
  "implementer": [
    {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"hello.txt": "bye\n"}},
    {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"hello.txt": "hello\n"}}
  ],
  "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]
}`
	root, tf := taskRepo(t, script, 1)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitNotVerified || !strings.Contains(r.stdout, "hello: blocked") {
		t.Fatalf("first run: %+v", r)
	}
	r = run(t, root, nil, "task", "show", "hello")
	if !strings.Contains(r.stdout, "status    blocked") || !strings.Contains(r.stdout, "Last findings:") || !strings.Contains(r.stdout, "[gate]") {
		t.Fatalf("show blocked:\n%s", r.stdout)
	}
	// The fake script is consumed per process, so the retry replays it from the start:
	// point it at a script whose first step passes.
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(passScript), 0o644); err != nil {
		t.Fatal(err)
	}
	r = run(t, root, nil, "task", "run", "--file", tf, "--retry")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello a2 implementer") {
		t.Fatalf("retry: %+v", r)
	}
}

func TestTaskRunErrors(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"task"}, ExitUsage, "Usage"},
		{[]string{"task", "bogus"}, ExitUsage, "unknown subcommand"},
		{[]string{"task", "run"}, ExitUsage, "--file is required"},
		{[]string{"task", "run", "--file", "/nonexistent.json"}, ExitError, "no such file"},
		{[]string{"task", "run", "--file", tf, "--retry"}, ExitError, "does not exist yet"},
		{[]string{"task", "show"}, ExitUsage, "Usage"},
		{[]string{"task", "show", "nope"}, ExitError, "not found"},
	}
	for _, c := range cases {
		r := run(t, root, nil, c.args...)
		if r.code != c.code || !strings.Contains(r.stdout+r.stderr, c.want) {
			t.Errorf("%v: code %d, want %d with %q\nstdout: %s\nstderr: %s", c.args, r.code, c.code, c.want, r.stdout, r.stderr)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"id":"x","title":"t","kind":"implement","spec":"s"}`), 0o644)
	if r := run(t, root, nil, "task", "run", "--file", bad); r.code != ExitError || !strings.Contains(r.stderr, "acceptance") {
		t.Fatalf("invalid task: %+v", r)
	}
}

func TestTaskRunNotInitialized(t *testing.T) {
	root := gitRepo(t)
	tf := filepath.Join(t.TempDir(), "task.json")
	os.WriteFile(tf, []byte(taskFile), 0o644)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitError || !strings.Contains(r.stderr, "mill init") {
		t.Fatalf("not initialized: %+v", r)
	}
}

func TestTaskList(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)

	r := run(t, root, nil, "task", "list")
	if r.code != ExitOK || strings.TrimSpace(r.stdout) != "no tasks" {
		t.Fatalf("empty list: %+v", r)
	}
	r = run(t, root, nil, "task", "list", "--json")
	var empty struct {
		Schema string            `json:"schema"`
		Tasks  []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &empty); err != nil || empty.Schema != "mill/task-list@1" || empty.Tasks == nil || len(empty.Tasks) != 0 {
		t.Fatalf("empty --json: %v\n%s", err, r.stdout)
	}

	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}

	r = run(t, root, nil, "task", "list")
	if r.code != ExitOK {
		t.Fatalf("list: %+v", r)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header and one row:\n%s", r.stdout)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "ID STATUS ATTEMPTS TOKENS COST TITLE" {
		t.Fatalf("header = %q", lines[0])
	}
	if got := strings.Join(strings.Fields(lines[1]), " "); got != "hello verified 1 1,500 0.05 Say hello" {
		t.Fatalf("row = %q", got)
	}

	r = run(t, root, nil, "task", "list", "--json")
	var doc struct {
		Schema string `json:"schema"`
		Tasks  []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Phase    string `json:"phase"`
			Attempts int    `json:"attempts"`
			Usage    struct {
				InputTokens int64   `json:"input_tokens"`
				CostUSD     float64 `json:"cost_usd"`
			} `json:"usage"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc.Schema != "mill/task-list@1" || len(doc.Tasks) != 1 {
		t.Fatalf("list --json: %v\n%s", err, r.stdout)
	}
	if tk := doc.Tasks[0]; tk.ID != "hello" || tk.Title != "Say hello" || tk.Status != "verified" || tk.Attempts != 1 || tk.Usage.InputTokens != 1200 || tk.Usage.CostUSD != 0.05 {
		t.Fatalf("task = %+v", tk)
	}

	// Status filter.
	r = run(t, root, nil, "task", "list", "--status", "blocked")
	if r.code != ExitOK || strings.TrimSpace(r.stdout) != "no tasks" {
		t.Fatalf("blocked filter: %+v", r)
	}
	r = run(t, root, nil, "task", "list", "--status", "verified")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello") {
		t.Fatalf("verified filter: %+v", r)
	}

	// Invalid status is a usage error naming the valid ones.
	r = run(t, root, nil, "task", "list", "--status", "bogus")
	if r.code != ExitUsage {
		t.Fatalf("bogus status code = %d", r.code)
	}
	for _, want := range []string{"pending", "running", "verified", "blocked", "failed"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %q: %s", want, r.stderr)
		}
	}

	if r := run(t, root, nil, "task"); !strings.Contains(r.stderr, "mill task list") {
		t.Errorf("usage lacks list: %s", r.stderr)
	}
}

func TestTaskShowAcceptsGoalTaskID(t *testing.T) {
	root, _ := taskRepo(t, passScript, 3)
	ctx := context.Background()
	p := project.PathsAt(root)
	s, err := store.Open(ctx, p.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl := plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: []taskspec.Spec{{
		ID: "config", Title: "Config", Kind: "implement", Spec: "do it", Acceptance: []string{"done"},
		Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}}}}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "task", "show", g.ID+"/config")
	if r.code != ExitOK || !strings.Contains(r.stdout, g.ID+"/config") || !strings.Contains(r.stdout, "pending") {
		t.Fatalf("show: %+v", r)
	}
}

// syncBuffer is a bytes.Buffer safe to write from one goroutine while another reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type asyncRun struct {
	stdout, stderr *syncBuffer
	done           chan int
}

// startTaskRun runs `mill task run` with args in a goroutine, polling the store every few
// milliseconds while it waits for a scheduler.
func startTaskRun(t *testing.T, ctx context.Context, root string, args ...string) *asyncRun {
	t.Helper()
	old := taskRunPollInterval
	taskRunPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { taskRunPollInterval = old })
	r := &asyncRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1)}
	go func() {
		r.done <- Run(ctx, Env{Stdout: r.stdout, Stderr: r.stderr, Dir: root}, append([]string{"task", "run"}, args...))
	}()
	return r
}

// waitFor polls cond until it holds, failing the test after a deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *asyncRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.done:
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("task run did not finish\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
		return 0
	}
}

func openTestStore(t *testing.T, root string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTaskRunWaitsForSchedulerHoldingRunLock(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	paths := project.PathsAt(root)
	l, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s := openTestStore(t, root)

	r := startTaskRun(t, ctx, root, "--file", tf)
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(r.stdout.String(), "queued hello") })
	if !strings.Contains(r.stdout.String(), "the running scheduler (pid "+strconv.Itoa(os.Getpid())+") will run it") {
		t.Fatalf("queued line: %s", r.stdout)
	}

	// The scheduler holding the lock picks the task up and runs it.
	cfg, err := config.Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	var schedOut syncBuffer
	p, err := newPipeline(Env{Stdout: &schedOut, Stderr: &schedOut, Dir: root}, "run", paths, cfg, "", s, &schedOut)
	if err != nil {
		t.Fatal(err)
	}
	sch := &scheduler.Scheduler{Store: s, MaxParallel: 1, RunTask: p.Run, Out: &schedOut}
	schedDone := make(chan error, 1)
	go func() {
		_, err := sch.Run(ctx, scheduler.RunOptions{PollInterval: 5 * time.Millisecond})
		schedDone <- err
	}()

	code := r.wait(t)
	if err := <-schedDone; err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	out := r.stdout.String()
	if code != ExitOK || !strings.Contains(out, "hello: verified at ") || !strings.Contains(out, "Review: git -C") {
		t.Fatalf("task run: code %d\nstdout: %s\nstderr: %s", code, out, r.stderr)
	}
	if strings.Contains(out, "implementer") {
		t.Fatalf("task run ran the task itself:\n%s", out)
	}
	atts, err := s.Attempts(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 || atts[0].Role != "implementer" || atts[1].Role != "verifier" {
		t.Fatalf("attempts = %+v, want one implementer and one verifier attempt", atts)
	}
}

func TestTaskRunWaitingForSchedulerReportsBlockedTask(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s := openTestStore(t, root)

	r := startTaskRun(t, ctx, root, "--file", tf)
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(r.stdout.String(), "queued hello") })
	if err := s.SetTaskStatus(ctx, "hello", store.TaskBlocked, "", "gate kept failing"); err != nil {
		t.Fatal(err)
	}
	code := r.wait(t)
	if code != ExitNotVerified || !strings.Contains(r.stdout.String(), "hello: blocked: gate kept failing") {
		t.Fatalf("task run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
}

func TestTaskRunRunsTaskItselfWhenSchedulerStops(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	r := startTaskRun(t, ctx, root, "--file", tf)
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(r.stdout.String(), "queued hello") })
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	code := r.wait(t)
	out := r.stdout.String()
	if code != ExitOK || !strings.Contains(out, "running it here") || !strings.Contains(out, "hello a1 implementer ok") ||
		!strings.Contains(out, "hello: verified") || !strings.Contains(out, "Review: git -C") {
		t.Fatalf("task run: code %d\nstdout: %s\nstderr: %s", code, out, r.stderr)
	}
}

func TestTaskRunWaitingLeavesOtherRunningAttemptsAndStaysQueuedOnInterrupt(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	bg := context.Background()
	s := openTestStore(t, root)
	other := taskspec.Spec{ID: "other", Title: "Other", Kind: "implement", Spec: "s", Acceptance: []string{"a"}}
	if err := s.CreateTask(bg, other); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(bg, "other", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(bg, "other", 1, "implementer"); err != nil {
		t.Fatal(err)
	}
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	r := startTaskRun(t, ctx, root, "--file", tf)
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(r.stdout.String(), "queued hello") })
	cancel()
	code := r.wait(t)
	if code != ExitError || !strings.Contains(r.stderr.String(), "hello stays queued") {
		t.Fatalf("interrupted task run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout.String(), "recovered") {
		t.Fatalf("task run recovered state under a live scheduler:\n%s", r.stdout)
	}
	o, err := s.GetTask(bg, "other")
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningAttempts(bg)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != store.TaskRunning || len(running) != 1 || running[0].TaskID != "other" {
		t.Fatalf("other task = %s, running attempts = %+v; want both left running", o.Status, running)
	}
	h, err := s.GetTask(bg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != store.TaskPending {
		t.Fatalf("hello = %s, want pending", h.Status)
	}
}

func TestTaskRunRetryRefusedWhileSchedulerRunsTask(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	s := openTestStore(t, root)
	spec, err := taskspec.Load(tf)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "hello", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "hello", 2, "implementer"); err != nil {
		t.Fatal(err)
	}
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	r := run(t, root, nil, "task", "run", "--file", tf, "--retry")
	if r.code != ExitError || !strings.Contains(r.stderr, "running attempt 2 (implementer)") ||
		!strings.Contains(r.stderr, strconv.Itoa(os.Getpid())) {
		t.Fatalf("retry of running task: %+v", r)
	}
	if h, err := s.GetTask(ctx, "hello"); err != nil || h.Status != store.TaskRunning {
		t.Fatalf("hello = %+v, %v; want still running", h, err)
	}
}

func TestTaskRunRetryUnderSchedulerRequeuesBlockedTask(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	s := openTestStore(t, root)
	spec, err := taskspec.Load(tf)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "hello", store.TaskBlocked, "", "stuck"); err != nil {
		t.Fatal(err)
	}
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := startTaskRun(t, cctx, root, "--file", tf, "--retry")
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(r.stdout.String(), "queued hello") })
	if h, err := s.GetTask(ctx, "hello"); err != nil || h.Status != store.TaskPending {
		t.Fatalf("hello = %+v, %v; want pending", h, err)
	}
	cancel()
	if code := r.wait(t); code != ExitError {
		t.Fatalf("interrupted: code %d", code)
	}
}

func TestTaskRunRecoversInterruptedTasks(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := taskspec.Load(tf)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "hello", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r := run(t, root, nil, "task", "run", "--file", tf, "--retry")
	if r.code != ExitOK || !strings.Contains(r.stdout, "recovered 1 interrupted task") {
		t.Fatalf("run: %+v", r)
	}
}

func TestTaskShowMarksEstimatedCosts(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	if r := run(t, root, nil, "task", "show", "hello"); strings.Contains(r.stdout, "~$") || strings.Contains(r.stdout, "estimated") {
		t.Fatalf("reported costs marked as estimated:\n%s", r.stdout)
	}

	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(ctx, "hello")
	if err != nil || len(attempts) == 0 {
		t.Fatalf("attempts = %v, %v", attempts, err)
	}
	a := attempts[0]
	if err := s.FinishAttempt(ctx, a.ID, a.Status, a.Usage, true, a.OutputPath, a.LogPath); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r := run(t, root, nil, "task", "show", "hello")
	if !strings.Contains(r.stdout, "~$") || !strings.Contains(r.stdout, "includes estimated costs") {
		t.Fatalf("show does not mark the estimate:\n%s", r.stdout)
	}
	r = run(t, root, nil, "task", "show", "hello", "--json")
	var v struct {
		Attempts []struct {
			CostEstimated bool `json:"cost_estimated"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || len(v.Attempts) == 0 || !v.Attempts[0].CostEstimated {
		t.Fatalf("json cost_estimated: %v\n%s", err, r.stdout)
	}
}

func TestRunningPublishedOrLandedTaskIsRefused(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	for _, st := range []string{store.TaskPublished, store.TaskLanded} {
		setStatus(t, root, "hello", st)
		if r := run(t, root, nil, "task", "run", "--file", tf, "--retry"); r.code != ExitError || !strings.Contains(r.stderr, "already "+st) {
			t.Errorf("retry of %s: %+v", st, r)
		}
		if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitError || !strings.Contains(r.stderr, "status "+st) {
			t.Errorf("re-run of %s: %+v", st, r)
		}
		if got, _ := openStore(t, root).GetTask(context.Background(), "hello"); got.Status != st {
			t.Errorf("status after refusal = %s, want %s", got.Status, st)
		}
	}
}

func TestTaskShowRunningAttemptShowsLiveUsage(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "hello", 99, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAttemptUsage(ctx, id, budget.Usage{InputTokens: 1234, OutputTokens: 56, CostUSD: 0.07}, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r := run(t, root, nil, "task", "show", "hello")
	if !strings.Contains(r.stdout, "running") || !strings.Contains(r.stdout, "1,234") || !strings.Contains(r.stdout, "~$0.07") {
		t.Fatalf("show lacks the running attempt's live usage:\n%s", r.stdout)
	}
}

func TestTaskRunRateLimitedExitsBlockedWithReason(t *testing.T) {
	resets := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	script := `{"implementer": [{"rate_limit": {"type": "five_hour", "resets_at": "` + resets.Format(time.RFC3339) + `"}},
	                            {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"hello.txt": "hello\n"}}],
	            "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]}`
	root, tf := taskRepo(t, script, 3)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitNotVerified {
		t.Fatalf("task run: %+v", r)
	}
	for _, want := range []string{"hello a1 implementer rate_limited",
		"hello: blocked: rate limited: the five_hour usage limit was reached; resets at ", resets.Local().Format("15:04")} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "hello a2") {
		t.Errorf("a second attempt ran after the rate limit:\n%s", r.stdout)
	}
}
