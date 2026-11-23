package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/store"
)

// verifiedGoal is a repo whose goal g1 ran to completion: a, b and c (c needs a and b) are verified.
func verifiedGoal(t *testing.T) string {
	t.Helper()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := run(t, root, nil, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	return root
}

func TestReviewListShowsVerifiedAndPublishedTasks(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "g1/a")
	br.PublishedRef = "refs/mill/review/g1/a"
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	setStatus(t, root, "g1/a", store.TaskPublished)
	setStatus(t, root, "g1/b", store.TaskLanded)

	r := run(t, root, nil, "review")
	if r.code != ExitOK {
		t.Fatalf("review: %+v", r)
	}
	for _, want := range []string{"ID", "STATUS", "ATTEMPTS", "COST", "TITLE", "g1/a", "published", "refs/mill/review/g1/a", "g1/c", "verified"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("list missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "g1/b") {
		t.Errorf("landed task listed:\n%s", r.stdout)
	}

	r = run(t, root, nil, "review", "--json")
	var doc struct {
		Schema string `json:"schema"`
		Tasks  []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			PublishedRef string `json:"published_ref"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc.Schema != "mill/review-list@1" || len(doc.Tasks) != 2 ||
		doc.Tasks[0].ID != "g1/a" || doc.Tasks[0].PublishedRef != "refs/mill/review/g1/a" {
		t.Fatalf("json = %+v, %v\n%s", doc, err, r.stdout)
	}
}

func TestReviewListEmpty(t *testing.T) {
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := run(t, root, nil, "review"); r.code != ExitOK || strings.TrimSpace(r.stdout) != "nothing to review" {
		t.Fatalf("review: %+v", r)
	}
	r := run(t, root, nil, "review", "--json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"mill/review-list@1"`) || !strings.Contains(r.stdout, `"tasks": []`) {
		t.Fatalf("review --json: %+v", r)
	}
}

func TestReviewShowPrintsSpecVerdictGateAndDiff(t *testing.T) {
	root := verifiedGoal(t)
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	for _, want := range []string{"write a.txt", "a.txt exists", "Last verdict: pass: ok", "Last gate: pass", "test -f a.txt",
		"mill/g1/a", "Diff stat", "a.txt | 1 +", "attempts  1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "+++ b/a.txt") {
		t.Errorf("stat output contains the full diff:\n%s", r.stdout)
	}

	r = run(t, root, nil, "review", "show", "g1/a", "--diff")
	if r.code != ExitOK || !strings.Contains(r.stdout, "+++ b/a.txt") || !strings.Contains(r.stdout, "+a") {
		t.Fatalf("show --diff: %+v", r)
	}

	r = run(t, root, nil, "review", "show", "g1/a", "--json")
	var v struct {
		Schema  string `json:"schema"`
		Verdict struct {
			Verdict string `json:"verdict"`
		} `json:"verdict"`
		Gate struct {
			Passed bool `json:"passed"`
		} `json:"gate"`
		DiffStat string `json:"diff_stat"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.Schema != "mill/review@1" || v.Verdict.Verdict != "pass" ||
		!v.Gate.Passed || !strings.Contains(v.DiffStat, "a.txt") || v.Attempts != 1 {
		t.Fatalf("json = %+v, %v\n%s", v, err, r.stdout)
	}
}

func TestReviewShowIncludesGateEventsOfLastAttempt(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	if _, err := s.AppendEvent(ctx, "gate-flaky", "", "g1/a", map[string]any{"attempt": 1, "commands": []string{"flaky-cmd"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "owns-outside", "", "g1/a", map[string]any{"attempt": 1, "files": []string{"stray.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "gate-base-failing", "", "g1/a", map[string]any{"attempt": 0, "commands": []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "gate-flaky") || !strings.Contains(r.stdout, "flaky-cmd") ||
		!strings.Contains(r.stdout, "owns-outside") || strings.Contains(r.stdout, "gate-base-failing") {
		t.Fatalf("show: %+v", r)
	}
}

func TestReviewRefusesTasksNotVerifiedOrPublished(t *testing.T) {
	root := verifiedGoal(t)
	setStatus(t, root, "g1/a", store.TaskBlocked)
	for _, args := range [][]string{
		{"review", "show", "g1/a"},
		{"review", "changes", "g1/a", "-m", "x"},
		{"review", "reject", "g1/a", "-m", "x"},
	} {
		r := run(t, root, nil, args...)
		if r.code != ExitError || !strings.Contains(r.stderr, "is blocked") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a"); tk.Status != store.TaskBlocked || tk.Reason != "test" {
		t.Errorf("refused command changed the task: %+v", tk)
	}
}

func TestReviewChangesRecordsOneOperatorFindingPerParagraph(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	if r := run(t, root, nil, "review", "changes", "g1/a", "-m", "Rename the file.\n\nAdd a comment\nover two lines.\n\n\n"); r.code != ExitOK ||
		!strings.Contains(r.stdout, "mill run g1") {
		t.Fatalf("changes: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g1/a")
	if tk.Status != store.TaskPending || tk.Reason != "changes requested" {
		t.Fatalf("task = %+v", tk)
	}
	fs, err := pipeline.LastFindings(ctx, s, "g1/a")
	if err != nil || len(fs) != 2 || fs[0].Source != "operator" || fs[0].Issue != "Rename the file." ||
		fs[1].Issue != "Add a comment\nover two lines." {
		t.Fatalf("findings = %+v, %v", fs, err)
	}
	evs, _ := s.TaskEvents(ctx, "g1/a")
	found := false
	for _, e := range evs {
		var d struct {
			Attempt int `json:"attempt"`
		}
		if e.Kind == "attempt-findings" && json.Unmarshal(e.Data, &d) == nil && d.Attempt == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("no attempt-findings event for attempt 1: %+v", evs)
	}
}

func TestReviewChangesFromFileAndPublishedNote(t *testing.T) {
	root := verifiedGoal(t)
	setStatus(t, root, "g1/b", store.TaskPublished)
	p := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(p, []byte("one\r\n\r\ntwo\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "changes", "g1/b", "--file", p)
	if r.code != ExitOK || !strings.Contains(r.stdout, "2 finding(s)") || !strings.Contains(r.stdout, "stays published") {
		t.Fatalf("changes: %+v", r)
	}
	if r := run(t, root, nil, "review", "changes", "g1/c"); r.code != ExitUsage {
		t.Errorf("changes without text: %+v", r)
	}
	if r := run(t, root, nil, "review", "changes", "g1/c", "-m", " \n\n "); r.code != ExitError {
		t.Errorf("changes with empty text: %+v", r)
	}
}

func TestReviewChangesOnSingleTaskNamesRetryCommand(t *testing.T) {
	root := soloRepo(t, `{"solo.txt": "s\n"}`)
	setStatus(t, root, "solo", store.TaskVerified)
	r := run(t, root, nil, "review", "changes", "solo", "-m", "tidy up")
	if r.code != ExitOK || !strings.Contains(r.stdout, "mill task run --file <file> --retry") {
		t.Fatalf("changes: %+v", r)
	}
}

func TestReviewRejectBlocksTaskAndHoldsNonLandedDescendants(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	r := run(t, root, nil, "review", "reject", "g1/a", "-m", "wrong approach")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g1/c") || strings.Contains(r.stdout, "g1/b") {
		t.Fatalf("reject: %+v", r)
	}
	a, _ := s.GetTask(ctx, "g1/a")
	c, _ := s.GetTask(ctx, "g1/c")
	b, _ := s.GetTask(ctx, "g1/b")
	if a.Status != store.TaskBlocked || a.Reason != "rejected: wrong approach" ||
		c.Status != store.TaskBlocked || c.Reason != "on hold: dependency g1/a was rejected" || b.Status != store.TaskVerified {
		t.Fatalf("a=%+v b=%+v c=%+v", a, b, c)
	}
	if r := run(t, root, nil, "review", "reject", "g1/b"); r.code != ExitUsage {
		t.Errorf("reject without reason: %+v", r)
	}
}

func TestReviewRejectLeavesLandedDescendantsAlone(t *testing.T) {
	root := verifiedGoal(t)
	setStatus(t, root, "g1/c", store.TaskLanded)
	r := run(t, root, nil, "review", "reject", "g1/a", "-m", "no")
	if r.code != ExitOK || !strings.Contains(r.stdout, "No other task depends on it") {
		t.Fatalf("reject: %+v", r)
	}
	if c, _ := openStore(t, root).GetTask(context.Background(), "g1/c"); c.Status != store.TaskLanded {
		t.Errorf("landed task changed: %+v", c)
	}
}

func TestReviewChangesReopensDoneGoalAndRunRerunsTask(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalDone {
		t.Fatalf("goal before = %+v", g)
	}
	r := run(t, root, nil, "review", "changes", "g1/c", "-m", "Tidy up.")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Goal g1 was done and is reopened") || !strings.Contains(r.stdout, "`mill run g1`") {
		t.Fatalf("changes: %+v", r)
	}
	g, _ := s.GetGoal(ctx, "g1")
	if g.Status != store.GoalApproved || !strings.Contains(g.Reason, "g1/c") || !strings.Contains(g.Reason, "changes requested") {
		t.Fatalf("goal after = %+v", g)
	}
	if r := run(t, root, nil, "run", "g1"); r.code != ExitOK || !strings.Contains(r.stdout, "g1/c: started") || strings.Contains(r.stdout, "g1/a: started") {
		t.Fatalf("rerun: %+v", r)
	}
	if tk, _ := s.GetTask(ctx, "g1/c"); tk.Status != store.TaskVerified {
		t.Fatalf("task = %+v", tk)
	}
	if g, _ := s.GetGoal(ctx, "g1"); g.Status != store.GoalDone {
		t.Fatalf("goal after rerun = %+v", g)
	}
}

func TestReviewRejectReopensDoneGoal(t *testing.T) {
	root := verifiedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	r := run(t, root, nil, "review", "reject", "g1/c", "-m", "wrong approach")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Goal g1 was done and is reopened") {
		t.Fatalf("reject: %+v", r)
	}
	g, _ := s.GetGoal(ctx, "g1")
	if g.Status != store.GoalApproved || !strings.Contains(g.Reason, "g1/c") || !strings.Contains(g.Reason, "rejected") {
		t.Fatalf("goal = %+v", g)
	}
	if tk, _ := s.GetTask(ctx, "g1/c"); tk.Status != store.TaskBlocked {
		t.Fatalf("task = %+v", tk)
	}
	if r := run(t, root, nil, "run", "g1", "--retry-blocked"); r.code != ExitOK || !strings.Contains(r.stdout, "g1/c: started") {
		t.Fatalf("rerun: %+v", r)
	}
}

// assertNoReviewClaims fails if any task claim row is left in the store. The store it
// opens treats every claim as alive: the claims are made by the test binary, whose
// command line the default liveness check does not recognise, so it would hide them.
func assertNoReviewClaims(t *testing.T, root string) {
	t.Helper()
	s, err := store.Open(context.Background(), project.PathsAt(root).DB, store.Options{
		ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	claims, err := s.ClaimedTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("claims left behind: %+v", claims)
	}
}

// holdRunLock takes .mill/run.lock as an unrelated mill run would, until the test ends.
func holdRunLock(t *testing.T, root string) {
	t.Helper()
	l, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Error(err)
		}
	})
}

// runningAttempt starts a verifier attempt of id (which keeps the task's approval) and
// returns its number.
func runningAttempt(t *testing.T, s *store.Store, id string) (int64, int) {
	t.Helper()
	ctx := context.Background()
	n, err := s.MaxAttemptN(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	aid, err := s.StartAttempt(ctx, id, n+1, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	return aid, n + 1
}

// workerProcess is a live process standing in for the worker of another task's attempt.
type workerProcess struct {
	pid    int
	exited chan struct{}
}

func startWorker(t *testing.T, s *store.Store, attemptID int64) workerProcess {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := workerProcess{pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() { cmd.Wait(); close(w.exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-w.exited })
	if err := s.SetAttemptProcess(context.Background(), attemptID, w.pid, "sleep 300", ""); err != nil {
		t.Fatal(err)
	}
	return w
}

// assertWorkerUntouched fails unless the attempt of task id is still running and its
// worker process is alive.
func assertWorkerUntouched(t *testing.T, s *store.Store, id string, w workerProcess) {
	t.Helper()
	select {
	case <-w.exited:
		t.Errorf("worker pid %d of %s was killed", w.pid, id)
	default:
	}
	atts, err := s.Attempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) == 0 || atts[len(atts)-1].Status != store.AttemptRunning || atts[len(atts)-1].PID != w.pid {
		t.Errorf("attempts of %s changed: %+v", id, atts)
	}
}

// taskRows is everything a refused command must leave as it was.
type taskRows struct {
	Task     store.Task
	Branch   store.Branch
	Attempts []store.Attempt
	Events   int
}

func snapshotTask(t *testing.T, s *store.Store, id string) taskRows {
	t.Helper()
	ctx := context.Background()
	var r taskRows
	var err error
	if r.Task, err = s.GetTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r.Branch, err = s.GetBranch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r.Attempts, err = s.Attempts(ctx, id); err != nil {
		t.Fatal(err)
	}
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Events = len(evs)
	return r
}

func TestReviewCommandsRefuseTaskWithRunningAttempt(t *testing.T) {
	root := verifiedGoal(t)
	addRemote(t, root)
	s := openStore(t, root)
	_, n := runningAttempt(t, s, "g1/a")
	otherID, _ := runningAttempt(t, s, "g1/b")
	w := startWorker(t, s, otherID)
	before := snapshotTask(t, s, "g1/a")

	busy := fmt.Sprintf("g1/a is busy: attempt %d (verifier) running", n)
	for _, c := range []struct {
		prefix string
		args   []string
	}{
		{"mill review approve: ", []string{"review", "approve", "g1/a"}},
		{"mill review changes: ", []string{"review", "changes", "g1/a", "-m", "redo it"}},
		{"mill review reject: ", []string{"review", "reject", "g1/a", "-m", "wrong"}},
		{"mill task rebase: ", []string{"task", "rebase", "g1/a"}},
	} {
		r := run(t, root, nil, c.args...)
		if r.code != ExitError || !strings.Contains(r.stderr, c.prefix+"task "+busy) {
			t.Errorf("%v: %+v", c.args, r)
		}
		if after := snapshotTask(t, s, "g1/a"); !reflect.DeepEqual(after, before) {
			t.Errorf("%v changed the task:\nbefore %+v\nafter  %+v", c.args, before, after)
		}
		assertNoReviewClaims(t, root)
		assertWorkerUntouched(t, s, "g1/b", w)
	}
}

func TestReviewChangesAndRejectWorkWhileUnrelatedRunHoldsLock(t *testing.T) {
	root := verifiedGoal(t)
	s := openStore(t, root)
	holdRunLock(t, root)
	otherID, _ := runningAttempt(t, s, "g1/a")
	w := startWorker(t, s, otherID)

	if r := run(t, root, nil, "review", "changes", "g1/b", "-m", "rename it"); r.code != ExitOK {
		t.Fatalf("changes: %+v", r)
	}
	if r := run(t, root, nil, "review", "reject", "g1/c", "-m", "wrong"); r.code != ExitOK {
		t.Fatalf("reject: %+v", r)
	}
	ctx := context.Background()
	if tk, _ := s.GetTask(ctx, "g1/b"); tk.Status != store.TaskPending {
		t.Errorf("g1/b = %+v", tk)
	}
	if tk, _ := s.GetTask(ctx, "g1/c"); tk.Status != store.TaskBlocked {
		t.Errorf("g1/c = %+v", tk)
	}
	assertNoReviewClaims(t, root)
	assertWorkerUntouched(t, s, "g1/a", w)
}
