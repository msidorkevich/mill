package syncer

import (
	"context"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

const (
	syncExclPoll     = time.Millisecond
	syncExclDeadline = 20 * time.Second
	// syncExclGrace is how long the restack must keep waiting for the held worktree.
	syncExclGrace = 300 * time.Millisecond
)

type syncExclOutcome struct {
	res Result
	err error
}

// syncExclSync runs Sync in the background and returns its outcome on a channel.
func syncExclSync(o Options) <-chan syncExclOutcome {
	ch := make(chan syncExclOutcome, 1)
	go func() {
		res, err := Sync(context.Background(), o)
		ch <- syncExclOutcome{res, err}
	}()
	return ch
}

// syncExclActivity reads the activity of the live claim on task id, or "" when it is not
// claimed. The test binary runs no mill command, so every claim's process counts as alive.
func syncExclActivity(t *testing.T, o Options, id string) string {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, o.Paths.DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	claims, err := s.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return claims[id].Activity
}

// The syncer restacks a task only while it holds the task's worktree: while another holder
// has it, the restack waits and leaves the branch alone, and it completes once the worktree
// is released.
func TestWorktreeExclusiveSyncRestackWaitsForHeldWorktree(t *testing.T) {
	t.Parallel()
	o := stackedGoal(t, true)
	ctx := context.Background()
	br, err := o.Store.GetBranch(ctx, "g1/b")
	if err != nil {
		t.Fatal(err)
	}
	_, unlock, err := gitx.LockWorktree(ctx, br.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := syncExclSync(o)

	deadline := time.Now().Add(syncExclDeadline)
	for syncExclActivity(t, o, "g1/b") != "sync: restack" && time.Now().Before(deadline) {
		time.Sleep(syncExclPoll)
	}
	if got := syncExclActivity(t, o, "g1/b"); got != "sync: restack" {
		t.Fatalf("b's restack never started (activity %q)", got)
	}
	grace := time.Now().Add(syncExclGrace)
	for time.Now().Before(grace) {
		select {
		case out := <-done:
			t.Fatalf("Sync finished while the worktree was held: %+v, %v", out.res, out.err)
		default:
		}
		if head := git(t, br.Worktree, "rev-parse", "HEAD"); head != br.HeadSHA {
			t.Fatalf("b moved to %s while its worktree was held", head)
		}
		time.Sleep(syncExclPoll)
	}
	unlock()

	var out syncExclOutcome
	select {
	case out = <-done:
	case <-time.After(syncExclDeadline):
		t.Fatal("Sync did not finish after the worktree was released")
	}
	if out.err != nil {
		t.Fatal(out.err)
	}
	if len(out.res.Restacked) != 1 || out.res.Restacked[0].TaskID != "g1/b" {
		t.Fatalf("restacked = %+v, want g1/b", out.res.Restacked)
	}
	if head := git(t, br.Worktree, "rev-parse", "HEAD"); head == br.HeadSHA || head != out.res.Restacked[0].NewHead {
		t.Fatalf("b is at %s, want the restacked head %s", head, out.res.Restacked[0].NewHead)
	}
}
