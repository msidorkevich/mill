package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

func TestVerificationProceedsWhenBaseCommitIsMissing(t *testing.T) {
	t.Parallel()
	e := ownedTaskEnv(t, func(string) error { return nil }, verdict("pass"), verdict("pass"))
	const id = "owner"
	ctx := context.Background()
	if out := e.run(t, id); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	// Point the recorded base at a commit that is not in the repository, then verify again:
	// the verifier reads the branch record from the store.
	br, err := e.p.Store.GetBranch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	br.BaseSHA = strings.Repeat("0", 40)
	if err := e.p.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if _, stop, err := e.p.Verify(ctx, id); err != nil || stop != "" {
		t.Fatalf("Verify: stop=%q err=%v\n%s", stop, err, e.out)
	}
	// The missing commit makes git fail while listing the base's changes: the log names the
	// task, the attempt and the error, and says the verifier runs without them.
	const want = "owner a2 base changes: list the files changed on main in 0000000000000000000000000000000000000000.."
	if !strings.Contains(e.out.String(), want) || !strings.Contains(e.out.String(), "; verifying without them\n") {
		t.Fatalf("log lacks the base changes message %q:\n%s", want, e.out)
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 2 {
		t.Fatalf("verifier prompts = %d", len(ps))
	}
	if strings.Contains(ps[1], "Changed on the base since this task started") {
		t.Fatalf("prompt has a base-changes section:\n%s", ps[1])
	}
	if events := baseChangedEvents(t, e, id); len(events) != 0 {
		t.Fatalf("base-changed events = %v", events)
	}
}

func TestBaseChangesFailsWhenEventCannotBeRecorded(t *testing.T) {
	t.Parallel()
	e := ownedTaskEnv(t, commitOwnedFileOnBase, verdict("pass"))
	const id = "owner"
	ctx := context.Background()
	if out := e.run(t, id); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	task, err := e.p.Store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	br, err := e.p.Store.GetBranch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// The base changes can still be found, so the event write is the step that fails.
	if err := e.p.Store.Close(); err != nil {
		t.Fatal(err)
	}
	const want = "record the base changes of task owner"
	if _, err := e.p.baseChanges(ctx, task, br, 2); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("baseChanges: err = %v, want %q", err, want)
	}
}
