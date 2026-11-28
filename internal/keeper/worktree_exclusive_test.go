package keeper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// keepExclGrace is how long the overlap test keeps the worktree after the restack of t3
// started without the restack failing: a restack that ignores the lock fails at once on the
// index.lock, one that honours it waits.
const keepExclGrace = 300 * time.Millisecond

// keepExclIndexLock is the path of the index.lock of the worktree wt.
func keepExclIndexLock(t *testing.T, wt string) string {
	t.Helper()
	p := git(t, wt, "rev-parse", "--git-path", "index.lock")
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt, p)
	}
	return p
}

// keepExclWriteLock creates the index.lock of wt, as a git command in progress holds it, with
// modification time mtime.
func keepExclWriteLock(t *testing.T, wt string, mtime time.Time) string {
	t.Helper()
	p := keepExclIndexLock(t, wt)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// keepExclActivity is the activity on the claim of task id, "" when it is not claimed. The
// claimer must have no command (store.NewClaimer("")), so that the claim of the test process
// counts as alive.
func keepExclActivity(s *store.Store, id string) string {
	claims, err := s.ClaimedTasks(context.Background())
	if err != nil {
		return ""
	}
	return claims[id].Activity
}

// keepExclPass runs Pass in the background and returns its outcome on the channel.
type keepExclOutcome struct {
	res Result
	err error
}

func keepExclPass(o Options) <-chan keepExclOutcome {
	ch := make(chan keepExclOutcome, 1)
	go func() {
		res, err := Pass(context.Background(), o)
		ch <- keepExclOutcome{res, err}
	}()
	return ch
}

// keepExclAssertRestacked checks the pass rebased every task of the stack and left them
// verified.
func keepExclAssertRestacked(t *testing.T, e *pipelinedRestackEnv, out keepExclOutcome) {
	t.Helper()
	if out.err != nil {
		t.Fatalf("Pass: %v\n%s", out.err, e.out)
	}
	if len(out.res.Blocked) != 0 || len(out.res.Rebased) != 3 {
		t.Fatalf("rebased = %+v, blocked = %+v; want all three rebased\n%s", out.res.Rebased, out.res.Blocked, e.out)
	}
	for _, id := range []string{"g1/t1", "g1/t2", "g1/t3"} {
		if tk := pipelinedRestackStatus(t, e.o.Store, id); tk.Status != store.TaskVerified {
			t.Errorf("%s is %s (%s), want verified\n%s", id, tk.Status, tk.Reason, e.out)
		}
	}
}

// A git command running in t3's worktree under the worktree lock while the keeper comes to
// restack t3: the restack waits for the worktree instead of failing on its index.lock.
func TestWorktreeExclusiveRestackWaitsForGitHoldingTheWorktree(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{})
	e.o.Claimer = store.NewClaimer("")
	ctx := context.Background()
	wt3 := e.wts["g1/t3"]
	_, unlock, err := gitx.LockWorktree(ctx, wt3)
	if err != nil {
		t.Fatal(err)
	}
	lock := keepExclWriteLock(t, wt3, time.Now())
	done := keepExclPass(e.o)

	deadline := time.Now().Add(20 * time.Second)
	for keepExclActivity(e.o.Store, "g1/t3") != ": restack" && time.Now().Before(deadline) {
		time.Sleep(pipelinedRestackPoll)
	}
	if got := keepExclActivity(e.o.Store, "g1/t3"); got != ": restack" {
		t.Fatalf("t3's restack never started (activity %q)\n%s", got, e.out)
	}
	grace := time.Now().Add(keepExclGrace)
	for time.Now().Before(grace) && pipelinedRestackStatus(t, e.o.Store, "g1/t3").Status != store.TaskBlocked {
		time.Sleep(pipelinedRestackPoll)
	}
	// The git command finishes: it removes its index.lock and gives the worktree up.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	unlock()

	keepExclAssertRestacked(t, e, <-done)
}

// An index.lock older than a minute that no git process holds is removed before the rebase,
// with a note in the output.
func TestWorktreeExclusiveRestackRemovesStaleIndexLock(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{})
	mtime := time.Now().Add(-time.Hour)
	lock := keepExclWriteLock(t, e.wts["g1/t3"], mtime)
	e.o.Now = func() time.Time { return mtime.Add(2 * time.Minute) }

	keepExclAssertRestacked(t, e, <-keepExclPass(e.o))
	if !strings.Contains(e.out.String(), "g1/t3: removed stale "+lock) {
		t.Errorf("output does not note the removed index.lock:\n%s", e.out)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale %s is still there (%v)", lock, err)
	}
}

// A fresh index.lock may belong to a running git: it is left alone and the restack of its
// task fails on it.
func TestWorktreeExclusiveRestackLeavesFreshIndexLock(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{})
	now := time.Now()
	lock := keepExclWriteLock(t, e.wts["g1/t3"], now)
	e.o.Now = func() time.Time { return now.Add(30 * time.Second) }

	out := <-keepExclPass(e.o)
	if out.err == nil || !strings.Contains(out.err.Error(), lock+"': File exists") {
		t.Errorf("Pass err = %v, want t3's restack failed on its index.lock\n%s", out.err, e.out)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("fresh %s was removed: %v", lock, err)
	}
	if strings.Contains(e.out.String(), "removed stale") {
		t.Errorf("output reports a removal:\n%s", e.out)
	}
}

// keepExclCountingHost counts the gate processes it starts.
type keepExclCountingHost struct {
	host.Host
	started atomic.Int32
}

func (h *keepExclCountingHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	h.started.Add(1)
	return h.Host.Start(ctx, spec)
}

// A gate whose parent failed is stopped, but not in the middle of its git reads of the
// worktree: git killed there can leave index.lock behind and fail the task's next restack.
// The reads finish, the gate itself does not run and the re-run reports the interruption.
func TestWorktreeExclusiveGateReadsFinishWhenParentFails(t *testing.T) {
	t.Parallel()
	e := pipelinedRestackKeep(t, 2, runner.Script{})
	ctx := context.Background()
	h := &keepExclCountingHost{Host: host.NewExec()}
	e.o.Host = h
	k := &keeper{Options: e.o, upstream: "origin/main", res: &Result{}}
	tk := pipelinedRestackStatus(t, e.o.Store, "g1/t3")
	br, err := e.o.Store.GetBranch(ctx, "g1/t3")
	if err != nil {
		t.Fatal(err)
	}
	gctx, cancel := context.WithCancel(ctx)
	cancel() // the parent's gate failed
	po := k.publishOptions(tk.ID)
	var steps []string
	po.GateActivity = func(step string) error {
		steps = append(steps, step)
		return nil
	}

	_, err = k.gateAtHead(ctx, gctx, po, tk, br)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("gateAtHead err = %v, want the re-run cancelled after its reads", err)
	}
	if len(steps) == 0 || steps[0] != "waiting for gate" {
		t.Errorf("gate steps = %v; want the reads done and the wait for a slot reached", steps)
	}
	if n := h.started.Load(); n != 0 {
		t.Errorf("the gate started %d processes after its parent failed", n)
	}
	if _, err := os.Stat(keepExclIndexLock(t, br.Worktree)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("index.lock left in %s (%v)", br.Worktree, err)
	}
}
