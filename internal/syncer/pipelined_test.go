package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const pipelinedRestackPoll = time.Millisecond

// pipelinedRestackHost runs gates with the exec host, counting how many run at once. A gate
// in a worktree listed in hold waits for its func first; one listed in fail exits 1.
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
		spec.Argv = []string{"/bin/sh", "-c", "exit 1"}
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

// pipelinedRestackWaitEvent polls until task id has an event of kind; it gives up after a
// deadline or once ctx is done.
func pipelinedRestackWaitEvent(ctx context.Context, s *store.Store, id, kind string) bool {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if pipelinedRestackSeq(s, id, kind) > 0 {
			return true
		}
		time.Sleep(pipelinedRestackPoll)
	}
	return false
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

// pipelinedRestackStack is a goal with tasks p <- t1 <- t2 <- t3, each based on its parent's
// head, and p moved by one more commit so that sync restacks t1, t2 and t3 in turn. It
// returns the options (gates run by the returned host) and the worktree of each task.
func pipelinedRestackStack(t *testing.T, maxGates int) (Options, *pipelinedRestackHost, map[string]string) {
	t.Helper()
	o, _ := fixture(t)
	ctx := context.Background()
	root := o.Paths.Root
	base := git(t, root, "rev-parse", "HEAD")
	ids := []string{"p", "t1", "t2", "t3"}
	pl := plan.Plan{Goal: "stack"}
	for i, id := range ids {
		sp := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"}
		if i > 0 {
			sp.DependsOn = []string{ids[i-1]}
		}
		pl.Tasks = append(pl.Tasks, sp)
	}
	raw, _ := json.Marshal(pl)
	g, err := o.Store.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Store.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	wts := map[string]string{}
	from, fromBranch := base, "main"
	var branches []store.Branch
	for _, id := range ids {
		wt := filepath.Join(t.TempDir(), id)
		name := "mill/g1/" + id
		git(t, root, "worktree", "add", "-q", "-b", name, wt, from)
		head := writeCommit(t, wt, id+".txt", "work on "+id)
		wts["g1/"+id] = wt
		branches = append(branches, store.Branch{TaskID: "g1/" + id, Name: name, Base: fromBranch, BaseSHA: from, StartSHA: from, HeadSHA: head, GateSHA: head, Worktree: wt})
		from, fromBranch = head, name
	}
	branches[0].HeadSHA = writeCommit(t, wts["g1/p"], "p2.txt", "more work on p")
	branches[0].GateSHA = branches[0].HeadSHA
	for _, br := range branches {
		if err := o.Store.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := o.Store.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	h := &pipelinedRestackHost{inner: host.NewExec(), hold: map[string]func(context.Context){}, fail: map[string]bool{}}
	o.Host = h
	o.MaxParallelGates = maxGates
	o.GateSlotPoll = pipelinedRestackPoll
	return o, h, wts
}

func pipelinedRestackIDs[T any](xs []T, id func(T) string) []string {
	var out []string
	for _, x := range xs {
		out = append(out, id(x))
	}
	return out
}

func TestPipelinedRestackRestacksChildWhileParentGates(t *testing.T) {
	t.Parallel()
	o, h, wts := pipelinedRestackStack(t, 1)
	// t1's gate holds until t2 is restacked: without pipelining it would wait out the deadline.
	h.hold[wts["g1/t1"]] = func(ctx context.Context) { pipelinedRestackWaitEvent(ctx, o.Store, "g1/t2", "task-restacked") }
	out := &bytes.Buffer{}
	o.Out = out
	ctx := context.Background()
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	restacked, gated := pipelinedRestackSeq(o.Store, "g1/t2", "task-restacked"), pipelinedRestackSeq(o.Store, "g1/t1", "task-gate")
	if restacked == 0 || gated == 0 || restacked > gated {
		t.Errorf("t2 restacked at seq %d, t1's gate finished at seq %d: want the restack first", restacked, gated)
	}
	if got := h.maxActive(); got > 1 {
		t.Errorf("%d gates ran at once, max_parallel_gates is 1", got)
	}
	want := []string{"g1/t1", "g1/t2", "g1/t3"}
	if got := pipelinedRestackIDs(res.Restacked, func(r Restacked) string { return r.TaskID }); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("restacked = %v, want %v", got, want)
	}
	doc, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if i1, i2, i3 := strings.Index(string(doc), `"task_id":"g1/t1"`), strings.Index(string(doc), `"task_id":"g1/t2"`), strings.Index(string(doc), `"task_id":"g1/t3"`); i1 < 0 || i1 > i2 || i2 > i3 {
		t.Errorf("json does not list t1, t2, t3 in stack order: %s", doc)
	}
	text := out.String()
	if i1, i2, i3 := strings.Index(text, "g1/t1: restacked"), strings.Index(text, "g1/t2: restacked"), strings.Index(text, "g1/t3: restacked"); i1 < 0 || i1 > i2 || i2 > i3 {
		t.Errorf("progress does not list t1, t2, t3 in stack order:\n%s", text)
	}
	for _, id := range want {
		tk, _ := o.Store.GetTask(ctx, id)
		br, _ := o.Store.GetBranch(ctx, id)
		if tk.Status != store.TaskVerified || tk.Phase != "" || br.GateSHA != br.HeadSHA {
			t.Errorf("%s: status %s phase %q gate %s head %s", id, tk.Status, tk.Phase, short(br.GateSHA), short(br.HeadSHA))
		}
	}
	if len(res.Blocked) != 0 {
		t.Errorf("blocked = %+v", res.Blocked)
	}
}

func TestPipelinedRestackGatesStayWithinMaxParallelGates(t *testing.T) {
	t.Parallel()
	o, h, wts := pipelinedRestackStack(t, 2)
	h.hold[wts["g1/t1"]] = func(ctx context.Context) { pipelinedRestackWaitEvent(ctx, o.Store, "g1/t3", "task-restacked") }
	res, err := Sync(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.maxActive(); got > 2 {
		t.Errorf("%d gates ran at once, max_parallel_gates is 2", got)
	}
	if len(res.Restacked) != 3 || len(res.Blocked) != 0 {
		t.Errorf("restacked %+v, blocked %+v", res.Restacked, res.Blocked)
	}
}

func TestPipelinedRestackParentGateFailureKeepsChildUnpublished(t *testing.T) {
	t.Parallel()
	o, h, wts := pipelinedRestackStack(t, 2)
	h.fail[wts["g1/t1"]] = true
	// t1's gate fails only once t2 is restacked, so t2's turn never finds t1 already blocked.
	h.hold[wts["g1/t1"]] = func(ctx context.Context) { pipelinedRestackWaitEvent(ctx, o.Store, "g1/t2", "task-restacked") }
	ctx := context.Background()
	// t2 is published: a red gate of t1 must keep it from being re-published.
	br, err := o.Store.GetBranch(ctx, "g1/t2")
	if err != nil {
		t.Fatal(err)
	}
	br.PublishedRef, br.PublishedSHA = "review/g1/t2", br.HeadSHA
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := o.Store.SetTaskStatus(ctx, "g1/t2", store.TaskPublished, "", ""); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	o.Out = out
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	// t3 is blocked too, or, when t2 was blocked before t3's turn came, left alone as a task
	// whose dependency is blocked.
	got := strings.Join(pipelinedRestackIDs(res.Blocked, func(b Blocked) string { return b.TaskID }), " ")
	if got != "g1/t1 g1/t2 g1/t3" && got != "g1/t1 g1/t2" {
		t.Fatalf("blocked = %+v, want t1, t2 (and t3) in stack order", res.Blocked)
	}
	if len(res.Republished) != 0 || len(res.NeedsRepublish) != 0 {
		t.Errorf("republished %+v, needs republish %+v", res.Republished, res.NeedsRepublish)
	}
	if pipelinedRestackSeq(o.Store, "g1/t2", "task-published") != 0 {
		t.Error("t2 was published")
	}
	if !strings.Contains(res.Blocked[1].Reason, "because the gate of its parent g1/t1 failed") {
		t.Errorf("t2's reason = %q", res.Blocked[1].Reason)
	}
	if !strings.Contains(out.String(), "g1/t2: blocked: sync: not gated or re-published after restacking onto mill/g1/t1 because the gate of its parent g1/t1 failed") {
		t.Errorf("output does not say why t2 was not published:\n%s", out)
	}
	tk, _ := o.Store.GetTask(ctx, "g1/t2")
	if tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskPublished {
		t.Errorf("t2: status %s, restack blocked from %q", tk.Status, tk.RestackBlockedFrom)
	}
}

func TestPipelinedRestackCancelReleasesGateSlots(t *testing.T) {
	t.Parallel()
	o, h, wts := pipelinedRestackStack(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// t1's gate holds its slot until the sync is cancelled, once t2 waits for a slot.
	h.hold[wts["g1/t1"]] = func(gctx context.Context) {
		if pipelinedRestackWaitEvent(gctx, o.Store, "g1/t2", "task-restacked") {
			pipelinedRestackWaitPhase(gctx, o.Store, "g1/t2", store.PhaseGateWait)
		}
		cancel()
		<-gctx.Done()
	}
	if _, err := Sync(ctx, o); err == nil {
		t.Fatal("a cancelled sync reported no error")
	}
	release, err := o.Store.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "check", PID: os.Getpid()}, 1, pipelinedRestackPoll,
		func() { t.Error("the only gate slot is still held after the cancelled sync") })
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

// pipelinedRestackWaitPhase polls until task id is in phase; it gives up after a deadline or
// once ctx is done.
func pipelinedRestackWaitPhase(ctx context.Context, s *store.Store, id, phase string) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if tk, err := s.GetTask(ctx, id); err == nil && tk.Phase == phase {
			return
		}
		time.Sleep(pipelinedRestackPoll)
	}
}
