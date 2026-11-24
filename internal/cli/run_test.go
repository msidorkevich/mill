package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// fanInPlan has two independent tasks and one that depends on both.
const fanInPlan = `{
  "goal": "Fan in",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": [], "size": "S"},
    {"id": "c", "title": "C", "kind": "implement", "spec": "write c.txt", "acceptance": ["all files exist"],
     "checks": ["test -f a.txt && test -f b.txt && test -f c.txt"], "owns": ["c.txt"], "depends_on": ["a", "b"], "size": "S"}
  ]
}`

// groupBPoll is the scheduler, task-run join and deadline-poll interval of these tests, so
// no test waits on a production default (seconds).
const groupBPoll = 5 * time.Millisecond

// groupBWatchInterval is the --interval given to mill watch in tests.
const groupBWatchInterval = "5ms"

// groupBRun runs mill in dir like run, with the scheduler and task-run join polling every
// groupBPoll instead of their production defaults.
func groupBRun(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return runEnv(t, dir, Env{SchedulerPoll: groupBPoll, TaskRunPoll: groupBPoll}, args...)
}

// taskSteps is a per-task fake script: the implementer writes files, the verifier passes.
func taskSteps(files string) string {
	return `{"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": ` + files + `}],
             "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]}`
}

// goalRepo makes an initialized repo with a commit on main, the given fake script and extra
// config, and the fan-in plan approved as g1.
func goalRepo(t *testing.T, script, extraCfg string) string {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "true"
max_attempts = 1
max_parallel = 2
worktree_root = "` + t.TempDir() + `"
` + extraCfg + `
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "plan", "--file", writePlan(t, fanInPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	return root
}

func fanInScript(aFiles string) string {
	return `{"tasks": {
  "g1/a": ` + taskSteps(aFiles) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `
}}`
}

func TestRunGoalFanInVerifiesAllTasks(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	r := groupBRun(t, root, "run", "g1")
	if r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"g1/a: started", "g1/b: started", "g1/c: started", "g1/c: verified",
		"goal g1: done", "ID  STATUS", "Done: all tasks are verified, on branches mill/g1/*."} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"g1/a", "g1/b", "g1/c"} {
		tk, err := s.GetTask(ctx, id)
		if err != nil || tk.Status != store.TaskVerified {
			t.Errorf("%s = %+v, %v", id, tk, err)
		}
		if !gitx.BranchExists(ctx, root, "mill/"+id) {
			t.Errorf("branch mill/%s missing", id)
		}
	}
	br, err := s.GetBranch(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(br.Worktree, f)); err != nil {
			t.Errorf("fan-in worktree lacks %s: %v", f, err)
		}
	}
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalDone {
		t.Errorf("goal = %+v", g)
	}

	if again := groupBRun(t, root, "run", "g1"); again.code != ExitError || !strings.Contains(again.stderr, "already done") {
		t.Errorf("run of done goal: %+v", again)
	}
}

func TestRunGoalBlockedTaskExitsNotVerified(t *testing.T) {
	t.Parallel()
	// a writes the wrong file, so its check fails and it blocks after one attempt.
	root := goalRepo(t, fanInScript(`{"wrong.txt": "x\n"}`), "")
	r := groupBRun(t, root, "run", "g1")
	if r.code != ExitNotVerified {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"Blocked: g1/a blocked (", "waiting: g1/c (needs g1/a)",
		"status    blocked", "mill run g1 --retry-blocked"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[string]string{"g1/a": store.TaskBlocked, "g1/b": store.TaskVerified, "g1/c": store.TaskPending} {
		if tk, err := s.GetTask(ctx, id); err != nil || tk.Status != want {
			t.Errorf("%s = %+v, %v; want %s", id, tk, err, want)
		}
	}
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalBlocked || !strings.Contains(g.Reason, "g1/a blocked") {
		t.Errorf("goal = %+v", g)
	}
}

func TestRunGoalRefusesPublishedStartMode(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), `start_children_on = "published"`)
	r := groupBRun(t, root, "run", "g1")
	if r.code != ExitError || !strings.Contains(r.stderr, `start_children_on = "verified"`) {
		t.Fatalf("run: %+v", r)
	}
}

func TestRunGoalRefusedWhileRunLockHeld(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	want := fmt.Sprintf("mill run: a scheduler is already running for this repository (pid %d); it picks up newly approved goals and queued tasks, follow it with `mill watch`", os.Getpid())
	for _, args := range [][]string{{"run", "g1"}, {"run"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitError || !strings.Contains(r.stderr, want) {
			t.Fatalf("%v while locked: %+v", args, r)
		}
		if strings.Contains(r.stdout, "started") {
			t.Errorf("%v started work while locked:\n%s", args, r.stdout)
		}
	}
}

func TestRunGoalRefusesDraftAndUsageErrors(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitError || !strings.Contains(r.stderr, "mill plan approve g1") {
		t.Fatalf("draft: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g1", "g2"); r.code != ExitUsage || !strings.Contains(r.stderr, "mill run [<goal>]") {
		t.Fatalf("two goals: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g1", "--bogus"); r.code != ExitUsage {
		t.Fatalf("bad flag: %+v", r)
	}
}

// limitedFanInScript makes g1/a hit a usage limit that reset at resetsAt, then succeed.
func limitedFanInScript(resetsAt time.Time) string {
	return `{"tasks": {
  "g1/a": {"implementer": [
             {"rate_limit": {"type": "five_hour", "resets_at": "` + resetsAt.Format(time.RFC3339) + `"}},
             {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"a.txt": "a\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `
}}`
}

func TestRunGoalRateLimitBlocksThenWaitOnLimitFinishes(t *testing.T) {
	t.Parallel()
	// The limit reset two minutes ago, so --wait-on-limit resumes at once.
	resets := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	root := goalRepo(t, limitedFanInScript(resets), "")
	r := groupBRun(t, root, "run", "g1")
	if r.code != ExitNotVerified {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"Blocked: rate limited: g1/a hit the account's usage limit; resets at ",
		resets.Local().Format("15:04"), "mill run g1 --retry-blocked", "--wait-on-limit"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "g1/c: started") {
		t.Errorf("g1/c started after the rate limit:\n%s", r.stdout)
	}

	r = groupBRun(t, root, "run", "g1", "--retry-blocked", "--wait-on-limit")
	if r.code != ExitOK {
		t.Fatalf("run --wait-on-limit: %+v", r)
	}
	resume := resets.Add(time.Minute).Local().Format("15:04")
	for _, want := range []string{"usage limit reached; waiting until " + resume + " to resume",
		"g1/a: back to pending (usage limit reset)", "goal g1: done"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
}

// twoGoalRepo is goalRepo with the fan-in plan approved a second time, as g2.
func twoGoalRepo(t *testing.T) string {
	t.Helper()
	script := `{"tasks": {
  "g1/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `,
  "g2/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g2/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g2/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `
}}`
	root := goalRepo(t, script, "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, fanInPlan)); r.code != ExitOK {
		t.Fatalf("plan g2: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve g2: %+v", r)
	}
	return root
}

func taskStatuses(t *testing.T, root string, ids ...string) map[string]string {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := map[string]string{}
	for _, id := range ids {
		tk, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got[id] = tk.Status
	}
	return got
}

func TestRunWithoutGoalRunsEveryApprovedGoal(t *testing.T) {
	t.Parallel()
	root := twoGoalRepo(t)
	r := groupBRun(t, root, "run")
	if r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	for _, want := range []string{"goal g1: done", "goal g2: done",
		"Done: all tasks are verified, on branches mill/g1/*.", "Done: all tasks are verified, on branches mill/g2/*."} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if n := strings.Count(r.stdout, "ID  STATUS"); n != 2 {
		t.Errorf("want 2 goal views, got %d:\n%s", n, r.stdout)
	}
	for id, st := range taskStatuses(t, root, "g1/a", "g1/b", "g1/c", "g2/a", "g2/b", "g2/c") {
		if st != store.TaskVerified {
			t.Errorf("%s = %s; want verified", id, st)
		}
	}
}

func TestRunGoalLeavesOtherGoalsPending(t *testing.T) {
	t.Parallel()
	root := twoGoalRepo(t)
	r := groupBRun(t, root, "run", "g1")
	if r.code != ExitOK {
		t.Fatalf("run g1: %+v", r)
	}
	if strings.Contains(r.stdout, "g2/") || strings.Contains(r.stdout, "mill/g2/*") {
		t.Errorf("run g1 touched g2:\n%s", r.stdout)
	}
	want := map[string]string{"g1/a": store.TaskVerified, "g1/b": store.TaskVerified, "g1/c": store.TaskVerified,
		"g2/a": store.TaskPending, "g2/b": store.TaskPending, "g2/c": store.TaskPending}
	for id, st := range taskStatuses(t, root, "g1/a", "g1/b", "g1/c", "g2/a", "g2/b", "g2/c") {
		if st != want[id] {
			t.Errorf("%s = %s; want %s", id, st, want[id])
		}
	}
}

func TestRunWithNothingReadyPrintsNothingToRun(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan)) // a draft goal is not ready
	r := groupBRun(t, root, "run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to run: no approved goal or queued task is ready.") {
		t.Fatalf("run: %+v", r)
	}
}

func TestRunRecoversInterruptedTasksWhenItBecomesScheduler(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	if r := groupBRun(t, root, "run"); r.code != ExitError || strings.Contains(r.stdout, "recovered") {
		t.Fatalf("run while locked recovered or did not fail: %+v", r)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}

	r := groupBRun(t, root, "run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "recovered 1 interrupted task(s)") {
		t.Fatalf("run: %+v", r)
	}
	if !strings.Contains(r.stdout, "goal g1: done") {
		t.Errorf("output missing goal g1: done:\n%s", r.stdout)
	}
}

func TestReadOnlyCommandsLeaveInterruptedTasksForRun(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Read-only commands never become the scheduler, so they never recover the interrupted
	// task.
	for _, args := range [][]string{{"goal", "show", "g1"}, {"goal", "list"}, {"task", "list"}} {
		r := run(t, root, nil, args...)
		if strings.Contains(r.stdout, "recovered") || strings.Contains(r.stderr, "recovered") {
			t.Errorf("mill %s printed recovery output: %+v", strings.Join(args, " "), r)
		}
	}
	if st := taskStatuses(t, root, "g1/a")["g1/a"]; st != store.TaskRunning {
		t.Fatalf("g1/a = %s after read-only commands; want %s", st, store.TaskRunning)
	}

	r := groupBRun(t, root, "run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "recovered 1 interrupted task(s)") {
		t.Fatalf("run: %+v", r)
	}
}

func TestRunReportsQueuedSingleTaskStatus(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 1)
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
	s.Close()
	r := groupBRun(t, root, "run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "task hello: verified") {
		t.Fatalf("run: %+v", r)
	}
}
