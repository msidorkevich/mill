package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mill/internal/pipeline"
	"mill/internal/store"
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
