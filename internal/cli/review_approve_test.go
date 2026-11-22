package cli

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// approvedEvents counts the task-approved events of a task.
func approvedEvents(t *testing.T, root, id string) int {
	t.Helper()
	evs, err := openStore(t, root).TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == "task-approved" {
			n++
		}
	}
	return n
}

func TestApproveQueuesChildOfUnpublishedParent(t *testing.T) {
	root, bare := chainPublished(t, false)
	before := remoteRefs(t, bare)

	r := run(t, root, nil, "review", "approve", "g2/b")
	want := "approved g2/b: queued, will publish when parent g2/a is published or landed\n"
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("approve child: %+v", r)
	}
	if refs := remoteRefs(t, bare); refs["refs/heads/review/g2/b"] != "" || refs["refs/heads/review/g2/a"] != "" {
		t.Fatalf("queued approve pushed: %v", refs)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
	tk, err := openStore(t, root).GetTask(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskVerified || !tk.Approved || tk.ApprovedAttempt < 1 {
		t.Fatalf("task = %s approved=%v attempt=%d", tk.Status, tk.Approved, tk.ApprovedAttempt)
	}
	if n := approvedEvents(t, root, "g2/b"); n != 1 {
		t.Fatalf("task-approved events = %d, want 1", n)
	}

	// Approving again refreshes the approval and prints the same line.
	r = run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("re-approve child: %+v", r)
	}
	if n := approvedEvents(t, root, "g2/b"); n != 2 {
		t.Fatalf("task-approved events after re-approve = %d, want 2", n)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Fatal("re-approve pushed the child")
	}
}

func TestReviewListAndShowQueuedApproval(t *testing.T) {
	root, _ := chainPublished(t, false)
	if r := run(t, root, nil, "review", "approve", "g2/b"); r.code != ExitOK {
		t.Fatalf("approve child: %+v", r)
	}
	tk, err := openStore(t, root).GetTask(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	reason := "parent g2/a is published or landed"

	r := run(t, root, nil, "review")
	if r.code != ExitOK || !strings.Contains(r.stdout, "WAITING FOR") {
		t.Fatalf("review: %+v", r)
	}
	var childLine, parentLine string
	for _, line := range strings.Split(r.stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "g2/b "):
			childLine = line
		case strings.HasPrefix(line, "g2/a "):
			parentLine = line
		}
	}
	if !strings.Contains(childLine, " approved ") || !strings.Contains(childLine, reason) {
		t.Errorf("child row = %q\n%s", childLine, r.stdout)
	}
	if !strings.Contains(parentLine, " verified ") || strings.Contains(parentLine, "approved") {
		t.Errorf("parent row = %q", parentLine)
	}

	r = run(t, root, nil, "review", "--json")
	var doc struct {
		Tasks []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Waiting string `json:"waiting"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	got := map[string][2]string{}
	for _, x := range doc.Tasks {
		got[x.ID] = [2]string{x.Status, x.Waiting}
	}
	if got["g2/b"] != [2]string{"approved", reason} || got["g2/a"] != [2]string{store.TaskVerified, ""} {
		t.Errorf("json tasks = %v", got)
	}

	approval := "approved " + tk.ApprovedAt + " at attempt " + strconv.Itoa(tk.ApprovedAttempt)
	r = run(t, root, nil, "review", "show", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, approval) || !strings.Contains(r.stdout, reason) {
		t.Errorf("show missing %q:\n%+v", approval, r)
	}
	r = run(t, root, nil, "review", "show", "g2/b", "--json")
	var view struct {
		Approval string `json:"approval"`
		Waiting  string `json:"waiting"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &view); err != nil || view.Approval != approval || view.Waiting != reason {
		t.Errorf("show json = %+v, %v\n%s", view, err, r.stdout)
	}
	if r := run(t, root, nil, "review", "show", "g2/a"); r.code != ExitOK || strings.Contains(r.stdout, "approval") {
		t.Errorf("unapproved show: %+v", r)
	}
}

// writeCheckCmd replaces the project's check_cmd.
func writeCheckCmd(t *testing.T, root, cmd string) {
	t.Helper()
	cfgPath := project.PathsAt(root).Config
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), `check_cmd = "true"`, `check_cmd = "`+cmd+`"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApproveGoalPublishesParentsFirstAndQueuesBlockedChild(t *testing.T) {
	// g1: c depends on a and b, so c waits for the other dependency to land.
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)

	r := run(t, root, nil, "review", "approve", "--goal", "g1")
	if r.code != ExitOK {
		t.Fatalf("approve --goal: %+v", r)
	}
	ia, ib := strings.Index(r.stdout, "Published g1/a "), strings.Index(r.stdout, "Published g1/b ")
	ic := strings.Index(r.stdout, "approved g1/c: queued, will publish when the other dependencies land")
	if ia < 0 || ib < 0 || ic < 0 || ic < ia || ic < ib {
		t.Fatalf("output order wrong:\n%s", r.stdout)
	}
	refs := remoteRefs(t, bare)
	if refs["refs/heads/review/g1/a"] == "" || refs["refs/heads/review/g1/b"] == "" || refs["refs/heads/review/g1/c"] != "" {
		t.Fatalf("remote refs = %v", refs)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g1/c"); !tk.Approved || tk.Status != store.TaskVerified {
		t.Errorf("g1/c = %s approved=%v", tk.Status, tk.Approved)
	}
}

func TestApproveGoalOnStackPublishesParentBeforeChild(t *testing.T) {
	root, bare := chainPublished(t, false)
	r := run(t, root, nil, "review", "approve", "--goal", "g2")
	if r.code != ExitOK {
		t.Fatalf("approve --goal: %+v", r)
	}
	// Once the parent is published in this invocation, the child can stack on it right away.
	ia, ib := strings.Index(r.stdout, "Published g2/a "), strings.Index(r.stdout, "Published g2/b ")
	if ia < 0 || ib < 0 || ib < ia || !strings.Contains(r.stdout, "into review/g2/a") {
		t.Fatalf("parent not published before child:\n%s", r.stdout)
	}
	refs := remoteRefs(t, bare)
	parentPub, childPub := refs["refs/heads/review/g2/a"], refs["refs/heads/review/g2/b"]
	if parentPub == "" || childPub == "" {
		t.Fatalf("remote refs = %v", refs)
	}
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", childPub); got != childPub+" "+parentPub {
		t.Errorf("child parents = %q, want %s", got, parentPub)
	}
}

func TestApproveRefusalByGateRecordsNoApproval(t *testing.T) {
	root, bare := publishedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "g1/a")
	br.GateSHA = ""
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	writeCheckCmd(t, root, "exit 7")

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "the gate must be green at the current head") || strings.Contains(r.stdout, "queued") {
		t.Fatalf("approve: %+v", r)
	}
	assertNotApproved(t, root, "g1/a")
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; ok {
		t.Error("failing gate pushed a branch")
	}
}

func TestApproveRefusalByConflictRecordsNoApproval(t *testing.T) {
	root, _ := publishedGoal(t)
	commitOnMain(t, root, "a.txt", "other\n")
	gitC(t, root, "push", "-q", "origin", "main")

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "no conflict with the base") || strings.Contains(r.stdout, "queued") {
		t.Fatalf("approve: %+v", r)
	}
	assertNotApproved(t, root, "g1/a")
}

func TestApproveSeveralIdsKeepsGoingAfterStatusRefusal(t *testing.T) {
	root, bare := publishedGoal(t)
	setStatus(t, root, "g1/a", store.TaskPending)

	r := run(t, root, nil, "review", "approve", "g1/a", "g1/b")
	if r.code != ExitError || !strings.Contains(r.stderr, "the task must be verified") || !strings.Contains(r.stdout, "Published g1/b ") {
		t.Fatalf("approve: %+v", r)
	}
	assertNotApproved(t, root, "g1/a")
	if refs := remoteRefs(t, bare); refs["refs/heads/review/g1/b"] == "" || refs["refs/heads/review/g1/a"] != "" {
		t.Errorf("remote refs = %v", refs)
	}
}

func TestApproveNeedsExactlyIdsOrGoal(t *testing.T) {
	root := verifiedGoal(t)
	for _, args := range [][]string{
		{"review", "approve"},
		{"review", "approve", "g1/a", "--goal", "g1"},
		{"review", "approve", "--goal", "g1", "g1/a"},
	} {
		r := run(t, root, nil, args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "Usage: mill review approve") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	assertNotApproved(t, root, "g1/a")
}

func assertNotApproved(t *testing.T, root, id string) {
	t.Helper()
	tk, err := openStore(t, root).GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Approved {
		t.Errorf("%s has an approval", id)
	}
	if n := approvedEvents(t, root, id); n != 0 {
		t.Errorf("%s has %d task-approved events", id, n)
	}
}
