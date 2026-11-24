package keeper

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/syncer"
)

func TestKeeperRestackConflictRecordsRestackBlock(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolverStep(nil)}, // leaves the conflict markers
	})
	e.moveMain(t, "shared.txt", mainSide)
	if res := e.pass(t); len(res.Blocked) != 1 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	tk, _ := e.o.Store.GetTask(context.Background(), taskID)
	if tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskVerified {
		t.Fatalf("task = %s from %q", tk.Status, tk.RestackBlockedFrom)
	}
	// The next pass retries it (without the resolver) and keeps it blocked.
	res := e.pass(t)
	if len(res.Sync.Blocked) != 1 || e.roleCalls(runner.RoleResolver) != 1 {
		t.Fatalf("second pass = %+v\n%s", res, e.out)
	}
}

func TestRestackBlockedApprovedTaskIsRetriedRestoredAndPublishedFromQueue(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	s := e.o.Store
	if err := s.ApproveTask(ctx, taskID, 1); err != nil {
		t.Fatal(err)
	}
	// main moves into a conflict and a sync restack blocked the approved task.
	e.moveMain(t, "shared.txt", mainSide)
	if err := s.BlockRestack(ctx, taskID, store.TaskVerified, "sync: restacking onto main conflicts in shared.txt"); err != nil {
		t.Fatal(err)
	}

	// Still conflicting: the sync step retries and keeps it blocked, without the resolver.
	res := e.pass(t)
	if len(res.Sync.Blocked) != 1 || res.Sync.Blocked[0].TaskID != taskID || len(res.Queue.Published) != 0 {
		t.Fatalf("first pass = %+v\n%s", res, e.out)
	}
	if tk, _ := s.GetTask(ctx, taskID); tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskVerified ||
		!strings.Contains(tk.Reason, "conflicts in shared.txt") {
		t.Fatalf("task = %s from %q: %q", tk.Status, tk.RestackBlockedFrom, tk.Reason)
	}

	// main takes its change back: the restack now replays cleanly.
	tip := e.moveMain(t, "shared.txt", original)
	res = e.pass(t)
	if len(res.Sync.Restored) != 1 || res.Sync.Restored[0] != (syncer.Restored{TaskID: taskID, Status: store.TaskVerified}) || len(res.Sync.Restacked) != 1 {
		t.Fatalf("second pass sync = %+v\n%s", res.Sync, e.out)
	}
	if len(res.Queue.Published) != 1 || res.Queue.Published[0].TaskID != taskID {
		t.Fatalf("queue = %+v\n%s", res.Queue, e.out)
	}
	for _, role := range []runner.Role{runner.RoleResolver, runner.RoleImplementer, runner.RoleVerifier} {
		if n := e.roleCalls(role); n != 0 {
			t.Errorf("%s calls = %d, want none", role, n)
		}
	}
	tk, _ := s.GetTask(ctx, taskID)
	if tk.Status != store.TaskPublished || tk.RestackBlockedFrom != "" {
		t.Fatalf("task = %s from %q", tk.Status, tk.RestackBlockedFrom)
	}
	if parent := git(t, e.wt, "rev-parse", "HEAD^"); parent != tip {
		t.Errorf("head's parent = %s, want main's tip %s", parent, tip)
	}
}

func TestBlockAfterResolutionFailsVerifierIsNotRetriedBySync(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"shared.txt": resolved})},
		runner.RoleVerifier: {verdictStep("fail")},
	})
	e.moveMain(t, "shared.txt", mainSide)
	if res := e.pass(t); len(res.Blocked) != 1 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	tk, _ := e.o.Store.GetTask(context.Background(), taskID)
	if tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != "" {
		t.Fatalf("task = %s from %q, want a block sync leaves alone", tk.Status, tk.RestackBlockedFrom)
	}
	e.moveMain(t, "shared.txt", original)
	res := e.pass(t)
	if len(res.Sync.Restacked)+len(res.Sync.Restored)+len(res.Sync.Blocked)+len(res.Rebased) != 0 {
		t.Fatalf("second pass = %+v\n%s", res, e.out)
	}
	if after, _ := e.o.Store.GetTask(context.Background(), taskID); after.Status != store.TaskBlocked || after.Reason != tk.Reason {
		t.Fatalf("task = %s %q", after.Status, after.Reason)
	}
}
