package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

const (
	rebaseExclPoll     = time.Millisecond
	rebaseExclDeadline = 20 * time.Second
	// rebaseExclGrace is how long the rebase must keep waiting for the held worktree.
	rebaseExclGrace = 300 * time.Millisecond
)

// rebaseExclClaimed reports whether task id is claimed. The test binary runs no mill command,
// so every claim's process counts as alive.
func rebaseExclClaimed(t *testing.T, root, id string) bool {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	claims, err := s.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := claims[id]
	return ok
}

// `mill task rebase` moves the branch only while it holds the task's worktree: while another
// holder has it the command waits, and when its context ends it fails naming the worktree and
// leaves the branch unchanged.
func TestTaskRebaseWaitsForHeldWorktreeAndFailsWhenContextEnds(t *testing.T) {
	t.Parallel()
	root := soloRepo(t, `{"solo.txt": "s\n"}`)
	ctx := context.Background()
	s := openStore(t, root)
	br, err := s.GetBranch(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	commitOnMain(t, root, "m.txt", "m\n")
	_, unlock, err := gitx.LockWorktree(ctx, br.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(rctx, Env{Stdout: &out, Stderr: &errb, Dir: root}, []string{"task", "rebase", "solo"})
	}()

	deadline := time.Now().Add(rebaseExclDeadline)
	for !rebaseExclClaimed(t, root, "solo") && time.Now().Before(deadline) {
		select {
		case code := <-done:
			t.Fatalf("task rebase finished (%d) before claiming the task\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
		default:
		}
		time.Sleep(rebaseExclPoll)
	}
	if !rebaseExclClaimed(t, root, "solo") {
		t.Fatal("task rebase never claimed the task")
	}
	grace := time.Now().Add(rebaseExclGrace)
	for time.Now().Before(grace) {
		select {
		case code := <-done:
			t.Fatalf("task rebase finished (%d) while the worktree was held\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
		default:
		}
		time.Sleep(rebaseExclPoll)
	}
	cancel()

	var code int
	select {
	case code = <-done:
	case <-time.After(rebaseExclDeadline):
		t.Fatal("task rebase did not return after its context ended")
	}
	if code != ExitError || !strings.Contains(errb.String(), "lock worktree "+br.Worktree) {
		t.Fatalf("code = %d, want %d with an error naming the worktree %s\nstdout: %s\nstderr: %s", code, ExitError, br.Worktree, out.String(), errb.String())
	}
	if head := gitC(t, br.Worktree, "rev-parse", "HEAD"); head != br.HeadSHA {
		t.Fatalf("the branch moved to %s, want it unchanged at %s", head, br.HeadSHA)
	}
}
