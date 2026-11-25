package keeper

import (
	"context"
	"encoding/json"
	"os"
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
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const pipelinedRestackPoll = time.Millisecond

// pipelinedRestackHost runs gates with the exec host, counting how many run at once. A gate
// in a worktree listed in hold waits for its func first; one in a worktree listed in fail fails
// while the worktree has no fixed.txt.
type pipelinedRestackHost struct {
	inner       host.Host
	mu          sync.Mutex
	active, max int
	hold        map[string]func(ctx context.Context)
	fail        map[string]bool
}

func (h *pipelinedRestackHost) Name() string { return "pipelinedRestack" }

func (h *pipelinedRestackHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	h.mu.Lock()
	h.active++
	if h.active > h.max {
		h.max = h.active
	}
	h.mu.Unlock()
	if w := h.hold[spec.Dir]; w != nil {
		w(ctx)
	}
	if h.fail[spec.Dir] {
		if _, err := os.Stat(filepath.Join(spec.Dir, "fixed.txt")); err != nil {
			spec.Argv = []string{"/bin/sh", "-c", "echo broken by the restack; exit 1"}
		}
	}
	hd, err := h.inner.Start(ctx, spec)
	if err != nil {
		h.done()
		return nil, err
	}
	return &pipelinedRestackHandle{Handle: hd, h: h}, nil
}

func (h *pipelinedRestackHost) done() {
	h.mu.Lock()
	h.active--
	h.mu.Unlock()
}

func (h *pipelinedRestackHost) maxActive() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.max
}

type pipelinedRestackHandle struct {
	host.Handle
	h *pipelinedRestackHost
}

func (p *pipelinedRestackHandle) Wait() (host.Exit, error) {
	e, err := p.Handle.Wait()
	p.h.done()
	return e, err
}

// pipelinedRestackSeq is the seq of the first event of kind for task id, 0 when there is none.
func pipelinedRestackSeq(s *store.Store, id, kind string) int64 {
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		return 0
	}
	for _, e := range evs {
		if e.Kind == kind {
			return int64(e.Seq)
		}
	}
	return 0
}

// pipelinedRestackWaitEvent polls until task id has an event of kind, up to a deadline or
// until ctx is done. It reports whether the event came.
func pipelinedRestackWaitEvent(ctx context.Context, s *store.Store, id, kind string) bool {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if pipelinedRestackSeq(s, id, kind) != 0 {
			return true
		}
		time.Sleep(pipelinedRestackPoll)
	}
	return false
}

// pipelinedRestackWaitPhase polls until task id is in phase, up to a deadline or until ctx
// is done.
func pipelinedRestackWaitPhase(ctx context.Context, s *store.Store, id, phase string) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if tk, err := s.GetTask(ctx, id); err == nil && tk.Phase == phase {
			return
		}
		time.Sleep(pipelinedRestackPoll)
	}
}

// pipelinedRestackEnv is a keeper setup with goal tasks t1 <- t2 <- t3. t1 is based on main and
// conflicts with main, which moved; rerere knows the resolution. The sync leaves the stack
// alone, so the keeper's own goal walk restacks t1 onto main and t2 and t3 onto their moved
// parents.
type pipelinedRestackEnv struct {
	o    Options
	h    *pipelinedRestackHost
	fake *runner.Fake
	out  *gateSlotPipelineWriter
	wts  map[string]string
}

func pipelinedRestackKeep(t *testing.T, maxGates int, script runner.Script) *pipelinedRestackEnv {
	t.Helper()
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".mill/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".gitignore")
	base := writeCommit(t, root, "shared.txt", original, "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")
	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ids := []string{"t1", "t2", "t3"}
	pl := plan.Plan{Goal: "stack"}
	for i, id := range ids {
		sp := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"}
		if i > 0 {
			sp.DependsOn = []string{ids[i-1]}
		}
		pl.Tasks = append(pl.Tasks, sp)
	}
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	wts := map[string]string{}
	from, fromBranch := base, "main"
	for _, id := range ids {
		tid := store.TaskID(g.ID, id)
		wt := filepath.Join(t.TempDir(), id)
		name := "mill/" + tid
		git(t, root, "worktree", "add", "-q", "-b", name, wt, from)
		head := writeCommit(t, wt, id+".txt", id+"\n", "work on "+id)
		if id == "t1" {
			head = writeCommit(t, wt, "shared.txt", taskSide, "t1 changes shared")
		}
		wts[tid] = wt
		if err := s.SaveBranch(ctx, store.Branch{TaskID: tid, Name: name, Base: fromBranch, BaseSHA: from, StartSHA: from, HeadSHA: head, GateSHA: head, Worktree: wt}); err != nil {
			t.Fatal(err)
		}
		if aid, err := s.StartAttempt(ctx, tid, 1, "implementer"); err != nil {
			t.Fatal(err)
		} else if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, "task-verdict", g.ID, tid, map[string]any{"attempt": 1, "head": head,
			"verdict": map[string]any{"verdict": "pass", "summary": "fine", "findings": []any{}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, tid, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
		from, fromBranch = head, name
	}

	// main moves shared.txt; rerere learns how t1 resolves against it.
	tip := writeCommit(t, root, "shared.txt", mainSide, "main moves shared")
	git(t, root, "push", "-q", "origin", "main")
	wt1 := wts["g1/t1"]
	t1Head := git(t, wt1, "rev-parse", "HEAD")
	st, err := gitx.RebaseStart(ctx, wt1, tip, base, true)
	if err != nil || st.Done {
		t.Fatalf("rebase = %+v, %v; want a stop", st, err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err = gitx.RebaseContinue(ctx, wt1, st.Files, true); err != nil || !st.Done {
		t.Fatalf("continue = %+v, %v", st, err)
	}
	git(t, wt1, "reset", "-q", "--hard", t1Head)

	cfg := config.Default()
	cfg.CheckCmd = "true"
	cfg.WorktreeRoot = t.TempDir()
	cfg.Runner.Kind = "fake"
	cfg.Runner.FakeScript = "unused"
	cfg.GateTimeout = config.Duration{Duration: 10 * time.Second}
	cfg.Budget.Task = budget.Limits{}
	cfg.GateCheckBase = false
	cfg.MaxAttempts = 2
	h := &pipelinedRestackHost{inner: host.NewExec(), hold: map[string]func(context.Context){}, fail: map[string]bool{}}
	fake := runner.NewFake(script)
	out := &gateSlotPipelineWriter{}
	p := &pipeline.Pipeline{Store: s, Runner: fake, Host: h, Cfg: cfg, Paths: paths, Home: t.TempDir(),
		Out: out, MaxParallelGates: maxGates, GateSlotPoll: pipelinedRestackPoll}
	return &pipelinedRestackEnv{
		o: Options{Store: s, Cfg: cfg, Paths: paths, Host: h, Pipeline: p, Out: out,
			MaxParallelGates: maxGates, GateSlotPoll: pipelinedRestackPoll},
		h: h, fake: fake, out: out, wts: wts,
	}
}

// pipelinedRestackAssertOrder checks the rebased tasks and their progress lines come in stack
// order, in the result and its JSON.
func pipelinedRestackAssertOrder(t *testing.T, e *pipelinedRestackEnv, res Result) {
	t.Helper()
	var got []string
	for _, r := range res.Rebased {
		got = append(got, r.TaskID)
	}
	var lines []string
	for _, l := range strings.Split(e.out.String(), "\n") {
		if strings.Contains(l, ": rebased onto ") {
			id, _, _ := strings.Cut(l, ":")
			lines = append(lines, id)
		}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rebased []struct {
			TaskID string `json:"task_id"`
		} `json:"rebased"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var js []string
	for _, r := range doc.Rebased {
		js = append(js, r.TaskID)
	}
	want := "g1/t1 g1/t2 g1/t3"
	if strings.Join(got, " ") != want || strings.Join(lines, " ") != want || strings.Join(js, " ") != want {
		t.Errorf("rebased = %v, output lines = %v, json = %v; want %s\n%s", got, lines, js, want, e.out)
	}
}

func pipelinedRestackStatus(t *testing.T, s *store.Store, id string) store.Task {
	t.Helper()
	tk, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestPipelinedRestackKeepRestacksChildWhileParentGates(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 1, runner.Script{})
	e.h.hold[e.wts["g1/t1"]] = func(ctx context.Context) {
		pipelinedRestackWaitEvent(ctx, e.o.Store, "g1/t2", "task-restacked")
	}
	res, err := Pass(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	restacked, gated := pipelinedRestackSeq(e.o.Store, "g1/t2", "task-restacked"), pipelinedRestackSeq(e.o.Store, "g1/t1", "task-gate")
	if restacked == 0 || gated == 0 || restacked > gated {
		t.Errorf("t2 restacked at seq %d, t1's gate finished at seq %d: want the restack first\n%s", restacked, gated, e.out)
	}
	if got := e.h.maxActive(); got != 1 {
		t.Errorf("%d gates ran at once, max_parallel_gates is 1", got)
	}
	if len(res.Blocked) != 0 || len(res.ResolvedRerere) != 1 {
		t.Errorf("blocked = %+v, resolved by rerere = %v\n%s", res.Blocked, res.ResolvedRerere, e.out)
	}
	pipelinedRestackAssertOrder(t, e, res)
}

func TestPipelinedRestackKeepGatesStayWithinMaxParallelGates(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{})
	// t1's gate runs until t3 is restacked, so t2's gate runs next to it and t3's waits.
	e.h.hold[e.wts["g1/t1"]] = func(ctx context.Context) {
		pipelinedRestackWaitEvent(ctx, e.o.Store, "g1/t3", "task-restacked")
	}
	res, err := Pass(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if got := e.h.maxActive(); got != 2 {
		t.Errorf("%d gates ran at once, want 2 (max_parallel_gates)", got)
	}
	if len(res.Blocked) != 0 {
		t.Errorf("blocked = %+v\n%s", res.Blocked, e.out)
	}
	pipelinedRestackAssertOrder(t, e, res)
}

func TestPipelinedRestackKeepParentGateFailureBlocksChild(t *testing.T) {
	t.Parallel()
	// The fixer cannot get t1's gate green: its change does not add fixed.txt.
	e := pipelinedRestackKeep(t, 2, runner.Script{
		runner.RoleImplementer: {implStep(map[string]string{"other.txt": "x\n"}), implStep(map[string]string{"other.txt": "y\n"})},
	})
	e.h.fail[e.wts["g1/t1"]] = true
	// t1's gate fails only once t2 is restacked, so the goal walk never finds t2's parent
	// already blocked.
	e.h.hold[e.wts["g1/t1"]] = func(ctx context.Context) {
		pipelinedRestackWaitEvent(ctx, e.o.Store, "g1/t2", "task-restacked")
	}
	t3Head := git(t, e.wts["g1/t3"], "rev-parse", "HEAD")
	remote := git(t, e.o.Paths.Root, "ls-remote", "origin")
	res, err := Pass(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if tk := pipelinedRestackStatus(t, e.o.Store, "g1/t1"); tk.Status != store.TaskBlocked {
		t.Errorf("t1 is %s, want blocked by the failed fix\n%s", tk.Status, e.out)
	}
	tk := pipelinedRestackStatus(t, e.o.Store, "g1/t2")
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "the gate of its parent g1/t1 failed") {
		t.Errorf("t2 is %s (%s), want blocked because t1's gate failed\n%s", tk.Status, tk.Reason, e.out)
	}
	if !strings.Contains(e.out.String(), "g1/t2: blocked: keeper: not gated or re-published after rebasing onto mill/g1/t1") {
		t.Errorf("output does not say why t2 was not published:\n%s", e.out)
	}
	if len(res.Republished) != 0 || len(res.Sync.Republished) != 0 {
		t.Errorf("republished = %+v %+v, want none", res.Republished, res.Sync.Republished)
	}
	var blocked []string
	for _, b := range res.Blocked {
		blocked = append(blocked, b.TaskID)
	}
	// t3 is restacked and blocked too, or, when t2 was blocked before the goal walk reached
	// t3, skipped as a task whose dependency is blocked.
	t3 := pipelinedRestackStatus(t, e.o.Store, "g1/t3")
	br, err := e.o.Store.GetBranch(context.Background(), "g1/t3")
	if err != nil {
		t.Fatal(err)
	}
	switch strings.Join(blocked, " ") {
	case "g1/t1 g1/t2 g1/t3":
		if t3.Status != store.TaskBlocked || !strings.Contains(t3.Reason, "the gate of its parent g1/t2 failed") {
			t.Errorf("t3 is %s (%s), want blocked because its parent failed\n%s", t3.Status, t3.Reason, e.out)
		}
		if br.GateSHA == br.HeadSHA {
			t.Errorf("t3 is gated at its restacked head %s\n%s", br.HeadSHA, e.out)
		}
	case "g1/t1 g1/t2":
		want := Skipped{TaskID: "g1/t3", Reason: "dependency g1/t2 is blocked; waiting for it"}
		if len(res.Skipped) != 1 || res.Skipped[0] != want {
			t.Errorf("skipped = %+v, want %+v\n%s", res.Skipped, want, e.out)
		}
		if t3.Status != store.TaskVerified || br.HeadSHA != t3Head {
			t.Errorf("t3 is %s at %s, want verified and left at %s\n%s", t3.Status, br.HeadSHA, t3Head, e.out)
		}
	default:
		t.Errorf("blocked = %v, want t1, t2 (and t3) in stack order\n%s", blocked, e.out)
	}
	for _, id := range []string{"g1/t2", "g1/t3"} {
		if pipelinedRestackSeq(e.o.Store, id, "task-published") != 0 {
			t.Errorf("%s was published\n%s", id, e.out)
		}
	}
	if got := git(t, e.o.Paths.Root, "ls-remote", "origin"); got != remote {
		t.Errorf("the remote's branches changed:\n%s\nwant:\n%s", got, remote)
	}
	// The fixer's progress lines come with t1's, before t2's.
	o := e.out.String()
	fixer, child := strings.Index(o, "g1/t1: the gate failed after the restack; running the fixer"), strings.Index(o, "g1/t2: rebased onto")
	if fixer < 0 || child < 0 || fixer > child {
		t.Errorf("fixer line at %d, t2's rebase line at %d: want t1's lines first\n%s", fixer, child, o)
	}
}

func TestPipelinedRestackKeepChildOfFixedParentIsRestackedOntoFix(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{
		runner.RoleImplementer: {implStep(map[string]string{"fixed.txt": "yes\n"})},
		runner.RoleVerifier:    {verdictStep("pass")},
	})
	e.h.fail[e.wts["g1/t1"]] = true
	// t1's gate fails only once t2 is restacked, so t2 sits on t1's head from before the fix
	// and the goal walk never finds t1 being fixed.
	e.h.hold[e.wts["g1/t1"]] = func(ctx context.Context) {
		pipelinedRestackWaitEvent(ctx, e.o.Store, "g1/t2", "task-restacked")
	}
	res, err := Pass(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if len(res.Fixed) != 1 || res.Fixed[0] != "g1/t1" || len(res.Blocked) != 0 {
		t.Fatalf("fixed = %v, blocked = %+v\n%s", res.Fixed, res.Blocked, e.out)
	}
	ctx := context.Background()
	prev := ""
	for _, id := range []string{"g1/t1", "g1/t2", "g1/t3"} {
		if tk := pipelinedRestackStatus(t, e.o.Store, id); tk.Status != store.TaskVerified {
			t.Errorf("%s is %s (%s), want verified\n%s", id, tk.Status, tk.Reason, e.out)
		}
		br, err := e.o.Store.GetBranch(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if prev != "" {
			if in, err := gitx.IsAncestor(ctx, e.o.Paths.Root, prev, br.HeadSHA); err != nil || !in {
				t.Errorf("%s at %s does not hold its parent's head %s (%v)\n%s", id, br.HeadSHA, prev, err, e.out)
			}
		}
		if br.GateSHA != br.HeadSHA {
			t.Errorf("%s gate %s, head %s: want the gate at the head", id, br.GateSHA, br.HeadSHA)
		}
		prev = br.HeadSHA
	}
	if !strings.Contains(e.out.String(), "g1/t2: its parent g1/t1 moved after this restack") {
		t.Errorf("output does not say t2 is restacked again:\n%s", e.out)
	}
}

func TestPipelinedRestackKeepCancelReleasesGateSlots(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 1, runner.Script{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// t1's gate holds the only slot until the pass is cancelled, once t2 waits for a slot.
	e.h.hold[e.wts["g1/t1"]] = func(gctx context.Context) {
		if pipelinedRestackWaitEvent(gctx, e.o.Store, "g1/t2", "task-restacked") {
			pipelinedRestackWaitPhase(gctx, e.o.Store, "g1/t2", store.PhaseGateWait)
		}
		cancel()
		<-gctx.Done()
	}
	if _, err := Pass(ctx, e.o); err == nil {
		t.Fatalf("a cancelled pass reported no error\n%s", e.out)
	}
	release, err := e.o.Store.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "check", PID: os.Getpid()}, 1, pipelinedRestackPoll,
		func() { t.Error("the only gate slot is still held after the cancelled pass") })
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
