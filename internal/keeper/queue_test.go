package keeper

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

func TestPassWithoutApprovalsHasEmptyQueue(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	res := e.pass(t)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"queue":{"published":[],"waiting":[]}`) {
		t.Errorf("result JSON lacks an empty queue: %s", raw)
	}
}

func TestPassPublishesApprovedTaskFromQueue(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	if err := e.o.Store.ApproveTask(ctx, taskID, 1); err != nil {
		t.Fatal(err)
	}
	res := e.pass(t)
	if len(res.Queue.Published) != 1 || res.Queue.Published[0].TaskID != taskID || res.Queue.Published[0].Ref != "review/"+taskID {
		t.Fatalf("queue = %+v\nlog:\n%s", res.Queue, e.out)
	}
	if got := git(t, e.bare, "rev-parse", "refs/heads/review/"+taskID); got != res.Queue.Published[0].SHA {
		t.Errorf("remote review branch at %s, queue says %s", got, res.Queue.Published[0].SHA)
	}
	if tk, _ := e.o.Store.GetTask(ctx, taskID); tk.Status != store.TaskPublished {
		t.Errorf("status = %s, want published", tk.Status)
	}
}

func TestPassClearsApprovalOfOlderAttempt(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	s := e.o.Store
	if err := s.ApproveTask(ctx, taskID, 1); err != nil {
		t.Fatal(err)
	}
	// A verifier attempt keeps the approval in the store, but it is not of the latest attempt.
	id, err := s.StartAttempt(ctx, taskID, 2, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, id, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	res := e.pass(t)
	if len(res.Queue.Published) != 0 || len(res.Queue.Waiting) != 0 {
		t.Errorf("queue = %+v", res.Queue)
	}
	tk, err := s.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Approved || tk.Status != store.TaskVerified {
		t.Errorf("task = %s approved=%v, want verified without approval", tk.Status, tk.Approved)
	}
	if _, err := gitx.Run(ctx, e.bare, "rev-parse", "--verify", "-q", "refs/heads/review/"+taskID); err == nil {
		t.Error("stale approval was published")
	}
	evs, err := s.TaskEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Kind == "task-approval-cleared" && strings.Contains(string(ev.Data), "attempt 1") {
			found = true
		}
	}
	if !found {
		t.Error("no task-approval-cleared event naming the old attempt")
	}
}
