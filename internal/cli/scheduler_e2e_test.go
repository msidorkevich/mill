package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// These tests drive one `mill run` (cli.Run in a goroutine) as the repository's scheduler
// while other mill commands run against the same temporary repository. Tasks are held in
// their gate by a check that waits for a release file the test creates, so the test decides
// when each task ends instead of sleeping.

// gates hands out barrier checks and releases them.
type gates struct{ dir string }

func newGates(t *testing.T) *gates {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &gates{dir: dir}
}

// hold is a task check that waits until name (or every barrier) is released.
func (g *gates) hold(name string) string {
	return "while [ ! -e " + filepath.Join(g.dir, name) + " ] && [ ! -e " + filepath.Join(g.dir, "all") + " ]; do sleep 0.01; done"
}

func (g *gates) release(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(g.dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// schedTask is one plan task: it writes file and its check waits on barrier (none when "").
type schedTask struct {
	id, file, barrier string
	owns              []string
	deps              []string
}

// schedPlan renders a plan whose tasks write their file and check it, after their barrier.
func schedPlan(t *testing.T, g *gates, goal string, tasks ...schedTask) string {
	t.Helper()
	pl := plan.Plan{Goal: goal, Assumptions: []string{}}
	for _, x := range tasks {
		check := "test -f " + x.file
		if x.barrier != "" {
			check = g.hold(x.barrier) + "; " + check
		}
		owns := x.owns
		if owns == nil {
			owns = []string{x.file}
		}
		deps := x.deps
		if deps == nil {
			deps = []string{}
		}
		pl.Tasks = append(pl.Tasks, taskspec.Spec{ID: x.id, Title: x.id, Kind: "implement", Spec: "write " + x.file,
			Acceptance: []string{x.file + " exists"}, Checks: []string{check}, Owns: owns, DependsOn: deps, Size: "S"})
	}
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// schedScript is a per-task fake script: each task writes files[id] (a file name) once.
func schedScript(files map[string]string) string {
	var parts []string
	for id, f := range files {
		parts = append(parts, `"`+id+`": `+taskSteps(`{"`+f+`": "x\n"}`))
	}
	return `{"tasks": {` + strings.Join(parts, ",\n") + `}}`
}

// schedRepo makes an initialized repo with a commit on main, the fake script and the given
// max_parallel, without goals.
func schedRepo(t *testing.T, script string, maxParallel int) string {
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
max_parallel = ` + itoa(maxParallel) + `
worktree_root = "` + t.TempDir() + `"
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
	return root
}

// approvePlan plans and approves body as the goal id the repository gives it next.
func approvePlan(t *testing.T, root, id, body string) {
	t.Helper()
	if r := run(t, root, nil, "plan", "--file", writePlan(t, body)); r.code != ExitOK || !strings.Contains(r.stdout, id) {
		t.Fatalf("plan %s: %+v", id, r)
	}
	if r := run(t, root, nil, "plan", "approve", id); r.code != ExitOK {
		t.Fatalf("plan approve %s: %+v", id, r)
	}
}

// startRun starts `mill run` in a goroutine. At cleanup every barrier is released and the
// run is waited for, so no run outlives its test.
func startRun(t *testing.T, root string, g *gates) *asyncRun {
	t.Helper()
	r := &asyncRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1)}
	go func() {
		r.done <- Run(context.Background(), Env{Stdout: r.stdout, Stderr: r.stderr, Dir: root, SchedulerPoll: groupBPoll, TaskRunPoll: groupBPoll}, []string{"run"})
	}()
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(g.dir, "all"), nil, 0o644); err != nil {
			t.Error(err)
		}
		select {
		case code := <-r.done:
			r.done <- code
		case <-time.After(60 * time.Second):
			t.Errorf("mill run did not end\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
		}
	})
	return r
}

// finish waits for the run to end and returns its exit code.
func (r *asyncRun) finish(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.done:
		r.done <- code
		return code
	case <-time.After(60 * time.Second):
		t.Fatalf("mill run did not end\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
		return 0
	}
}

// statusOf reads a task's status and phase.
func statusOf(t *testing.T, s *store.Store, id string) (status, phase string) {
	t.Helper()
	tk, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status, tk.Phase
}

// waitHeld waits until every task in ids is running its gate (held at its barrier).
func waitHeld(t *testing.T, s *store.Store, r *asyncRun, ids ...string) {
	t.Helper()
	waitFor(t, strings.Join(ids, ", ")+" to be held in the gate", func() bool {
		for _, id := range ids {
			if st, ph := statusOf(t, s, id); st != store.TaskRunning || ph != store.PhaseGate {
				return false
			}
		}
		return true
	})
	if strings.Contains(r.stdout.String(), "mill error") {
		t.Fatalf("run reported a mill error:\n%s", r.stdout)
	}
}

// waitStatus waits until task id has status want.
func waitStatus(t *testing.T, s *store.Store, id, want string) {
	t.Helper()
	waitFor(t, id+" to be "+want, func() bool {
		st, _ := statusOf(t, s, id)
		return st == want
	})
}

// runningSet lists which of ids are running.
func runningSet(t *testing.T, s *store.Store, ids ...string) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		if st, _ := statusOf(t, s, id); st == store.TaskRunning {
			out = append(out, id)
		}
	}
	return out
}

// timeline replays the scheduler's "<id>: started" and "<id>: verified|blocked" lines and
// calls check with the tasks running after each start.
func timeline(out string, ids []string, check func(running map[string]bool)) {
	running := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		for _, id := range ids {
			rest, ok := strings.CutPrefix(line, id+": ")
			if !ok {
				continue
			}
			switch {
			case rest == "started":
				running[id] = true
				check(running)
			case rest == store.TaskVerified || strings.HasPrefix(rest, store.TaskBlocked) || strings.HasPrefix(rest, store.TaskFailed):
				delete(running, id)
			}
		}
	}
}

func TestTwoGoalsRunUnderOneScheduler(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	ids := []string{"g1/s", "g1/x", "g2/s", "g2/y"}
	// g1/s and g2/s own the same file; g2/y's check fails, so g2 ends blocked and g1 done.
	root := schedRepo(t, schedScript(map[string]string{"g1/s": "shared.txt", "g1/x": "x.txt", "g2/s": "shared.txt", "g2/y": "wrong.txt"}), 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "First",
		schedTask{id: "s", file: "shared.txt", barrier: "g1-s"},
		schedTask{id: "x", file: "x.txt", barrier: "g1-x"}))
	approvePlan(t, root, "g2", schedPlan(t, g, "Second",
		schedTask{id: "s", file: "shared.txt", barrier: "g2-s"},
		schedTask{id: "y", file: "y.txt", owns: []string{"y.txt", "wrong.txt"}, barrier: "g2-y"}))
	s := openTestStore(t, root)
	r := startRun(t, root, g)

	// max_parallel = 2 holds the ready g2/y back while both g1 tasks run.
	waitHeld(t, s, r, "g1/s", "g1/x")
	if got := runningSet(t, s, ids...); len(got) != 2 {
		t.Fatalf("running = %v, want only g1/s and g1/x", got)
	}

	// A free slot goes to g2/y, not to g2/s whose owns overlap the running g1/s.
	g.release(t, "g1-x")
	waitHeld(t, s, r, "g1/s", "g2/y")
	if st, _ := statusOf(t, s, "g2/s"); st != store.TaskPending {
		t.Fatalf("g2/s = %s while g1/s runs, want pending", st)
	}

	// With g2/y ended, a slot is free but g2/s still overlaps g1/s.
	g.release(t, "g2-y")
	waitStatus(t, s, "g2/y", store.TaskBlocked)
	if st, _ := statusOf(t, s, "g2/s"); st != store.TaskPending {
		t.Fatalf("g2/s = %s while g1/s runs, want pending", st)
	}
	g.release(t, "g1-s", "g2-s")

	code := r.finish(t)
	out := r.stdout.String()
	if code != ExitNotVerified {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, out, r.stderr)
	}
	for _, want := range []string{"goal g1: done", "goal g2: blocked", "Done: all tasks are verified, on branches mill/g1/*.",
		"Blocked: g2/y blocked ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if gs := strings.Index(out, "g2/s: started"); gs < 0 || gs < strings.Index(out, "g1/s: verified") {
		t.Errorf("g2/s started before g1/s ended:\n%s", out)
	}
	crossGoal := false
	timeline(out, ids, func(running map[string]bool) {
		if len(running) > 2 {
			t.Errorf("more than max_parallel tasks running: %v", running)
		}
		if running["g1/s"] && running["g2/s"] {
			t.Errorf("g1/s and g2/s ran at once though their owns overlap")
		}
		crossGoal = crossGoal || (running["g1/s"] && running["g2/y"])
	})
	if !crossGoal {
		t.Errorf("no task of g1 ran alongside one of g2:\n%s", out)
	}
	for id, want := range map[string]string{"g1": store.GoalDone, "g2": store.GoalBlocked} {
		if gl, err := s.GetGoal(context.Background(), id); err != nil || gl.Status != want {
			t.Errorf("goal %s = %+v, %v; want %s", id, gl, err, want)
		}
	}
}

// approveRepo has g1 (a) and g2 (p, then q held at its barrier), a bare remote, and a run
// that has verified g1/a and g2/p and is holding g2/q.
func approveRepo(t *testing.T) (root, bare string, g *gates, s *store.Store, r *asyncRun) {
	t.Helper()
	g = newGates(t)
	root = schedRepo(t, schedScript(map[string]string{"g1/a": "a.txt", "g2/p": "p.txt", "g2/q": "q.txt"}), 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "First", schedTask{id: "a", file: "a.txt"}))
	approvePlan(t, root, "g2", schedPlan(t, g, "Second",
		schedTask{id: "p", file: "p.txt"},
		schedTask{id: "q", file: "q.txt", barrier: "g2-q", deps: []string{"p"}}))
	bare = addRemote(t, root)
	s = openTestStore(t, root)
	r = startRun(t, root, g)
	waitHeld(t, s, r, "g2/q")
	waitStatus(t, s, "g1/a", store.TaskVerified)
	waitStatus(t, s, "g2/p", store.TaskVerified)
	return root, bare, g, s, r
}

func TestApproveDuringRunLeavesOtherGoalRunning(t *testing.T) {
	t.Parallel()
	root, bare, g, s, r := approveRepo(t)

	// Goal A's verified task publishes and lands while goal B's task runs.
	a := run(t, root, nil, "review", "approve", "g1/a")
	if a.code != ExitOK || !strings.Contains(a.stdout, "Published g1/a") {
		t.Fatalf("approve g1/a during the run: %+v", a)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; !ok {
		t.Error("approve did not push review/g1/a")
	}
	mergeReview(t, forge(t, bare), "g1/a")
	sy := run(t, root, nil, "sync")
	if sy.code != ExitOK || !strings.Contains(sy.stdout, "Landed:\n  g1/a  ") || strings.Contains(sy.stdout, "Busy") {
		t.Fatalf("sync during the run: %+v", sy)
	}
	if st, _ := statusOf(t, s, "g1/a"); st != store.TaskLanded {
		t.Errorf("g1/a = %s, want landed", st)
	}
	if st, ph := statusOf(t, s, "g2/q"); st != store.TaskRunning || ph != store.PhaseGate {
		t.Fatalf("g2/q = %s (%s) after approve and sync, want still running its gate", st, ph)
	}

	// Approving the running task is refused as busy and changes nothing.
	q := run(t, root, nil, "review", "approve", "g2/q")
	if q.code != ExitError || !strings.Contains(q.stderr, "mill review approve: task g2/q is busy: ") ||
		!strings.Contains(q.stderr, "retry when it finishes") {
		t.Fatalf("approve of the running g2/q: %+v", q)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/q"]; ok {
		t.Error("approve pushed the running g2/q")
	}
	if st, ph := statusOf(t, s, "g2/q"); st != store.TaskRunning || ph != store.PhaseGate {
		t.Fatalf("g2/q = %s (%s) after the refused approve, want still running its gate", st, ph)
	}

	g.release(t, "g2-q")
	if code := r.finish(t); code != ExitOK || !strings.Contains(r.stdout.String(), "goal g2: done") {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
	if st, _ := statusOf(t, s, "g2/q"); st != store.TaskVerified {
		t.Errorf("g2/q = %s, want verified", st)
	}
}

func TestSyncDuringRunReportsRunningChildBusy(t *testing.T) {
	t.Parallel()
	// Sync must list the running g2/q as busy and keep the landed parent's branch under it.
	root, bare, g, s, r := approveRepo(t)

	// The running g2/q's parent lands: sync would restack g2/q, which the run is working on.
	if p := run(t, root, nil, "review", "approve", "g2/p"); p.code != ExitOK || !strings.Contains(p.stdout, "Published g2/p") {
		t.Fatalf("approve g2/p during the run: %+v", p)
	}
	mergeReview(t, forge(t, bare), "g2/p")
	sy := run(t, root, nil, "sync")
	if !strings.Contains(sy.stdout, "Landed:\n  g2/p  ") {
		t.Fatalf("sync during the run did not land g2/p: %+v", sy)
	}
	if st, ph := statusOf(t, s, "g2/q"); st != store.TaskRunning || ph != store.PhaseGate {
		t.Fatalf("g2/q = %s (%s) after sync, want still running its gate", st, ph)
	}
	if sy.code != ExitError || !strings.Contains(sy.stdout, "Busy (being worked on; run `mill sync` again later):\n  g2/q  ") {
		t.Errorf("sync did not refuse the running g2/q as busy: %+v", sy)
	}

	g.release(t, "g2-q")
	if code := r.finish(t); code != ExitOK {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
}

func TestPlanApproveDuringRunStartsGoalInSameRun(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	root := schedRepo(t, schedScript(map[string]string{"g1/a": "a.txt", "g2/b": "b.txt"}), 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "First", schedTask{id: "a", file: "a.txt", barrier: "g1-a"}))
	s := openTestStore(t, root)
	r := startRun(t, root, g)
	waitHeld(t, s, r, "g1/a")

	approvePlan(t, root, "g2", schedPlan(t, g, "Second", schedTask{id: "b", file: "b.txt"}))
	waitStatus(t, s, "g2/b", store.TaskVerified)
	if st, _ := statusOf(t, s, "g1/a"); st != store.TaskRunning {
		t.Fatalf("g1/a = %s, want still running: g2/b must run in the same run", st)
	}
	if !strings.Contains(r.stdout.String(), "g2/b: started") {
		t.Errorf("the run did not start g2/b:\n%s", r.stdout)
	}

	g.release(t, "g1-a")
	code := r.finish(t)
	out := r.stdout.String()
	if code != ExitOK {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, out, r.stderr)
	}
	for _, want := range []string{"goal g1: done", "goal g2: done", "on branches mill/g2/*."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTaskRunDuringRunJoinsScheduler(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	root := schedRepo(t, schedScript(map[string]string{"g1/a": "a.txt", "hello": "hello.txt"}), 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "First", schedTask{id: "a", file: "a.txt", barrier: "g1-a"}))
	tf := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(tf, []byte(taskFile), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root)
	r := startRun(t, root, g)
	waitHeld(t, s, r, "g1/a")

	tr := startTaskRun(t, context.Background(), root, "--file", tf)
	waitFor(t, "the task to be queued", func() bool { return strings.Contains(tr.stdout.String(), "queued hello") })
	code := tr.wait(t)
	out := tr.stdout.String()
	if code != ExitOK || !strings.Contains(out, "will run it") || !strings.Contains(out, "hello: verified at ") {
		t.Fatalf("task run: code %d\nstdout: %s\nstderr: %s", code, out, tr.stderr)
	}
	if strings.Contains(out, "implementer") || strings.Contains(out, "running it here") {
		t.Errorf("task run ran the task itself:\n%s", out)
	}
	if !strings.Contains(r.stdout.String(), "hello: started") {
		t.Errorf("the scheduler did not start hello:\n%s", r.stdout)
	}
	if st, _ := statusOf(t, s, "g1/a"); st != store.TaskRunning {
		t.Fatalf("g1/a = %s, want still running", st)
	}

	g.release(t, "g1-a")
	if code := r.finish(t); code != ExitOK || !strings.Contains(r.stdout.String(), "task hello: verified") {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestStaleAttemptUnderLiveSchedulerWaitsForNextRun(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	// g0/x runs twice: once before the scheduler, once after the next run recovers it.
	script := `{"tasks": {
  "g1/x": {"implementer": [{"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"x.txt": "x\n"}},
                           {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"x.txt": "x\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}},
                        {"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g2/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "hello": ` + taskSteps(`{"hello.txt": "hello\n"}`) + `
}}`
	root := schedRepo(t, script, 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "Stale", schedTask{id: "x", file: "x.txt"}))
	if rr := groupBRun(t, root, "run"); rr.code != ExitOK {
		t.Fatalf("first run: %+v", rr)
	}
	approvePlan(t, root, "g2", schedPlan(t, g, "Live", schedTask{id: "a", file: "a.txt", barrier: "g2-a"}))
	addRemote(t, root)
	tf := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(tf, []byte(taskFile), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := openTestStore(t, root)
	r := startRun(t, root, g)
	waitHeld(t, s, r, "g2/a")

	// A killed run left g1/x running with an attempt whose worker pid is dead.
	if err := s.SetTaskStatus(ctx, "g1/x", store.TaskRunning, store.PhaseImplement, ""); err != nil {
		t.Fatal(err)
	}
	aid, err := s.StartAttempt(ctx, "g1/x", 3, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, aid, deadPID(t), "claude", ""); err != nil {
		t.Fatal(err)
	}
	assertStale := func(after string) {
		t.Helper()
		if st, ph := statusOf(t, s, "g1/x"); st != store.TaskRunning || ph != store.PhaseImplement {
			t.Fatalf("g1/x = %s (%s) after %s, want left running", st, ph, after)
		}
		running, err := s.RunningAttempts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range running {
			found = found || a.ID == aid
		}
		if !found {
			t.Fatalf("stale attempt %d is no longer running after %s: %+v", aid, after, running)
		}
	}
	noRecovery := func(cmd string, res result) {
		t.Helper()
		if strings.Contains(res.stdout+res.stderr, "recovered") {
			t.Errorf("%s recovered state under a live scheduler: %+v", cmd, res)
		}
		assertStale(cmd)
	}

	ap := run(t, root, nil, "review", "approve", "g1/x")
	if ap.code != ExitError || !strings.Contains(ap.stderr, "task g1/x is busy: attempt 3 (implementer) running") {
		t.Errorf("approve of the stale task: %+v", ap)
	}
	noRecovery("review approve", ap)
	sy := run(t, root, nil, "sync")
	if sy.code != ExitOK {
		t.Errorf("sync: %+v", sy)
	}
	noRecovery("sync", sy)
	tr := startTaskRun(t, ctx, root, "--file", tf)
	code := tr.wait(t)
	if code != ExitOK || !strings.Contains(tr.stdout.String(), "hello: verified at ") {
		t.Fatalf("task run: code %d\nstdout: %s\nstderr: %s", code, tr.stdout, tr.stderr)
	}
	noRecovery("task run", result{code, tr.stdout.String(), tr.stderr.String()})

	g.release(t, "g2-a")
	if code := r.finish(t); code != ExitOK || strings.Contains(r.stdout.String(), "recovered") {
		t.Fatalf("live run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
	assertStale("the live run ended")

	next := groupBRun(t, root, "run")
	if next.code != ExitOK || !strings.Contains(next.stdout, "recovered 1 interrupted task(s) and 1 running attempt(s)") ||
		!strings.Contains(next.stdout, "goal g1: done") {
		t.Fatalf("next run: %+v", next)
	}
	if st, _ := statusOf(t, s, "g1/x"); st != store.TaskVerified {
		t.Errorf("g1/x = %s after the next run, want verified", st)
	}
	running, err := s.RunningAttempts(ctx)
	if err != nil || len(running) != 0 {
		t.Errorf("running attempts after the next run = %+v, %v", running, err)
	}
}

func TestWatchAndGoalShowDuringRunListBothGoals(t *testing.T) {
	t.Parallel()
	g := newGates(t)
	root := schedRepo(t, schedScript(map[string]string{"g1/a": "a.txt", "g2/b": "b.txt"}), 2)
	approvePlan(t, root, "g1", schedPlan(t, g, "First", schedTask{id: "a", file: "a.txt", barrier: "g1-a"}))
	approvePlan(t, root, "g2", schedPlan(t, g, "Second", schedTask{id: "b", file: "b.txt", barrier: "g2-b"}))
	s := openTestStore(t, root)
	r := startRun(t, root, g)
	waitHeld(t, s, r, "g1/a", "g2/b")

	w := run(t, root, nil, "watch", "--once", "--interval", groupBWatchInterval)
	if w.code != ExitOK || !strings.Contains(w.stdout, "2 running") || strings.Contains(w.stdout, "nothing running") {
		t.Fatalf("watch --once: %+v", w)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(w.stdout, "\n") {
		if f := strings.Fields(line); len(f) > 2 {
			rows[f[0]] = f[2]
		}
	}
	for _, id := range []string{"g1/a", "g2/b"} {
		if rows[id] != store.PhaseGate {
			t.Errorf("watch row of %s has stage %q, want %s:\n%s", id, rows[id], store.PhaseGate, w.stdout)
		}
	}

	for goal, id := range map[string]string{"g1": "g1/a", "g2": "g2/b"} {
		gs := run(t, root, nil, "goal", "show", goal)
		if gs.code != ExitOK {
			t.Fatalf("goal show %s: %+v", goal, gs)
		}
		found := false
		for _, line := range strings.Split(gs.stdout, "\n") {
			f := strings.Fields(line)
			found = found || (len(f) > 1 && (f[0] == id || f[0] == strings.TrimPrefix(id, goal+"/")) && f[1] == store.TaskRunning)
		}
		if !found {
			t.Errorf("goal show %s does not list %s as running:\n%s", goal, id, gs.stdout)
		}
	}

	g.release(t, "g1-a", "g2-b")
	if code := r.finish(t); code != ExitOK {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", code, r.stdout, r.stderr)
	}
}
