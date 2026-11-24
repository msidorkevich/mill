package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// verifiedTask creates a task and moves it to verified.
func verifiedTask(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTask(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, id, TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
}

func approve(t *testing.T, s *Store, id string, attempt int) {
	t.Helper()
	if err := s.ApproveTask(context.Background(), id, attempt); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}

func addDep(t *testing.T, s *Store, child, parent string) {
	t.Helper()
	if _, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO deps (task_id, depends_on) VALUES (?, ?)`, child, parent); err != nil {
		t.Fatal(err)
	}
}

// eventsOfKind returns the data of a task's events of one kind.
func eventsOfKind(t *testing.T, s *Store, id, kind string) []map[string]any {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Kind != kind {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// assertCleared checks the task has no approval and exactly one task-approval-cleared event.
func assertCleared(t *testing.T, s *Store, id string) {
	t.Helper()
	got, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Approved || got.ApprovedAt != "" || got.ApprovedAttempt != 0 {
		t.Fatalf("%s still approved: %+v", id, got)
	}
	var at, attempt any
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT approved_at, approved_attempt FROM tasks WHERE id = ?`, id).Scan(&at, &attempt); err != nil {
		t.Fatal(err)
	}
	if at != nil || attempt != nil {
		t.Fatalf("%s approval columns = %v, %v, want NULL", id, at, attempt)
	}
	evs := eventsOfKind(t, s, id, "task-approval-cleared")
	if len(evs) != 1 || evs[0]["reason"] == "" {
		t.Fatalf("%s task-approval-cleared events = %+v", id, evs)
	}
}

func assertApproved(t *testing.T, s *Store, id string, attempt int) {
	t.Helper()
	got, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Approved || got.ApprovedAttempt != attempt || got.ApprovedAt == "" {
		t.Fatalf("%s approval = %+v", id, got)
	}
	if evs := eventsOfKind(t, s, id, "task-approval-cleared"); len(evs) != 0 {
		t.Fatalf("%s task-approval-cleared events = %+v", id, evs)
	}
}

func TestMigrateFromV7AddsApprovalColumnsKeepingRows(t *testing.T) {
	// Serial: swaps the package-level migrations and LatestVersion that every Open reads.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	saved := migrations
	migrations = saved[:7]
	LatestVersion = 7
	s, err := Open(ctx, path, Options{})
	migrations, LatestVersion = saved, len(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks (id, title, kind, spec, status, created_at, updated_at)
		VALUES ('t1', 'T', 'implement', 's', 'verified', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion || v < 8 {
		t.Fatalf("schema = %d, want %d", v, LatestVersion)
	}
	got, err := s2.GetTask(ctx, "t1")
	if err != nil || got.Title != "T" || got.Status != TaskVerified || got.Approved {
		t.Fatalf("task after upgrade = %+v, %v", got, err)
	}
	approve(t, s2, "t1", 2)
	assertApproved(t, s2, "t1", 2)
}

func TestApproveVerifiedTaskRecordsApprovalAndEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	verifiedTask(t, s, "t1")
	approve(t, s, "t1", 3)
	got, _ := s.GetTask(ctx, "t1")
	if !got.Approved || got.ApprovedAttempt != 3 || got.ApprovedAt != fixed.Format(timeLayout) {
		t.Fatalf("task = %+v", got)
	}
	evs := eventsOfKind(t, s, "t1", "task-approved")
	if len(evs) != 1 || evs[0]["attempt"] != float64(3) {
		t.Fatalf("task-approved events = %+v", evs)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"approved":true`) || !strings.Contains(string(b), `"approved_attempt":3`) {
		t.Fatalf("json = %s", b)
	}
	rev, err := s.ReviewTasks(ctx)
	if err != nil || len(rev) != 1 || !rev[0].Approved || rev[0].ApprovedAttempt != 3 || rev[0].ApprovedAt == "" {
		t.Fatalf("review tasks = %+v, %v", rev, err)
	}
}

func TestUnapprovedTaskJSONOmitsApproval(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	verifiedTask(t, s, "t1")
	got, _ := s.GetTask(context.Background(), "t1")
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "approved") {
		t.Fatalf("json = %s", b)
	}
}

func TestApproveRefusesPublishedAndPendingTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	verifiedTask(t, s, "pub")
	if err := s.SetTaskStatus(ctx, "pub", TaskPublished, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec("pend")); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][]string{
		"pub":  {"published", "never queued"},
		"pend": {"pending", "not verified"},
	} {
		before, _ := s.GetTask(ctx, id)
		err := s.ApproveTask(ctx, id, 1)
		if err == nil {
			t.Fatalf("approve %s accepted", id)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("approve %s err = %v, want it to mention %q", id, err, w)
			}
		}
		after, _ := s.GetTask(ctx, id)
		if after.Approved || after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("%s changed: %+v", id, after)
		}
		if evs := eventsOfKind(t, s, id, "task-approved"); len(evs) != 0 {
			t.Fatalf("%s task-approved events = %+v", id, evs)
		}
	}
}

func TestClearApprovalRecordsEventOnlyWhenApproved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	verifiedTask(t, s, "t1")
	if err := s.ClearApproval(ctx, "t1", "nothing to clear"); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfKind(t, s, "t1", "task-approval-cleared"); len(evs) != 0 {
		t.Fatalf("cleared events without approval = %+v", evs)
	}
	approve(t, s, "t1", 1)
	if err := s.ClearApproval(ctx, "t1", "operator withdrew"); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, s, "t1")
	if evs := eventsOfKind(t, s, "t1", "task-approval-cleared"); evs[0]["reason"] != "operator withdrew" {
		t.Fatalf("reason = %v", evs[0]["reason"])
	}
	if err := s.ClearApproval(ctx, "t1", "again"); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, s, "t1") // still exactly one event
}

func TestNewCodeOrPendingClearsApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, act := range map[string]func(t *testing.T, s *Store, id string){
		"SetTaskStatus pending": func(t *testing.T, s *Store, id string) {
			if err := s.SetTaskStatus(ctx, id, TaskPending, "", "retry"); err != nil {
				t.Fatal(err)
			}
		},
		"RequestChanges": func(t *testing.T, s *Store, id string) {
			if err := s.RequestChanges(ctx, id, 1, []string{"fix it"}, "operator requested changes"); err != nil {
				t.Fatal(err)
			}
		},
		"RejectTask": func(t *testing.T, s *Store, id string) {
			if _, err := s.RejectTask(ctx, id, "rejected"); err != nil {
				t.Fatal(err)
			}
		},
		"StartAttempt implementer": func(t *testing.T, s *Store, id string) {
			if _, err := s.StartAttempt(ctx, id, 2, "implementer"); err != nil {
				t.Fatal(err)
			}
		},
		"StartAttempt fixer": func(t *testing.T, s *Store, id string) {
			if _, err := s.StartAttempt(ctx, id, 2, "fixer"); err != nil {
				t.Fatal(err)
			}
		},
		"RecoverInterrupted": func(t *testing.T, s *Store, id string) {
			if err := s.SetTaskStatus(ctx, id, TaskRunning, PhaseImplement, ""); err != nil {
				t.Fatal(err)
			}
			if n, _, err := s.RecoverInterrupted(ctx, nil); err != nil || n != 1 {
				t.Fatalf("recovered %d, %v", n, err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			verifiedTask(t, s, "t1")
			approve(t, s, "t1", 1)
			act(t, s, "t1")
			assertCleared(t, s, "t1")
		})
	}
}

func TestRejectTaskClearsApprovalOfHeldDescendants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []string{"a", "b", "c"} {
		verifiedTask(t, s, id)
	}
	addDep(t, s, "b", "a")
	addDep(t, s, "c", "b")
	approve(t, s, "b", 1)
	approve(t, s, "c", 1)
	if _, err := s.RejectTask(ctx, "a", "wrong approach"); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, s, "b")
	assertCleared(t, s, "c")
	if evs := eventsOfKind(t, s, "a", "task-approval-cleared"); len(evs) != 0 {
		t.Fatalf("unapproved a got cleared events = %+v", evs)
	}
}

func TestResetGoalTasksClearsApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	g, err := s.CreateGoal(ctx, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, goalPlan()); err != nil {
		t.Fatal(err)
	}
	a := TaskID(g.ID, "a")
	if err := s.SetTaskStatus(ctx, a, TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	approve(t, s, a, 1)
	ids, err := s.ResetGoalTasks(ctx, g.ID, []string{TaskVerified}, "", "replanned")
	if err != nil || len(ids) != 1 || ids[0] != a {
		t.Fatalf("reset = %v, %v", ids, err)
	}
	assertCleared(t, s, a)
}

func TestVerifierAttemptAndSaveBranchKeepApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	verifiedTask(t, s, "t1")
	approve(t, s, "t1", 1)
	if _, err := s.StartAttempt(ctx, "t1", 1, "verifier"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, Branch{TaskID: "t1", Name: "mill/t1", Base: "main", BaseSHA: "b2", HeadSHA: "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, Branch{TaskID: "t1", Name: "mill/t1", Base: "main", BaseSHA: "b3", HeadSHA: "h3"}); err != nil {
		t.Fatal(err)
	}
	assertApproved(t, s, "t1", 1)
}

func TestApprovedTasksAreVerifiedAndParentsComeFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	// Ids sort children before parents so only the dependency order can put parents first.
	// Stack: z-base <- a-top. Fan-in: x-left, y-right <- b-merge.
	for _, id := range []string{"a-top", "z-base", "b-merge", "x-left", "y-right", "c-unapproved", "d-published"} {
		verifiedTask(t, s, id)
	}
	addDep(t, s, "a-top", "z-base")
	addDep(t, s, "b-merge", "x-left")
	addDep(t, s, "b-merge", "y-right")
	for _, id := range []string{"a-top", "z-base", "b-merge", "x-left", "y-right", "d-published"} {
		approve(t, s, id, 1)
	}
	if err := s.SetTaskStatus(ctx, "d-published", TaskPublished, "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := s.ApprovedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	pos := map[string]int{}
	for i, tk := range got {
		if !tk.Approved || tk.Status != TaskVerified {
			t.Fatalf("listed %+v", tk)
		}
		ids = append(ids, tk.ID)
		pos[tk.ID] = i
	}
	want := []string{"x-left", "y-right", "b-merge", "z-base", "a-top"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("approved = %v, want %v", ids, want)
	}
	for _, d := range [][2]string{{"a-top", "z-base"}, {"b-merge", "x-left"}, {"b-merge", "y-right"}} {
		if pos[d[1]] > pos[d[0]] {
			t.Fatalf("%s listed before its dependency %s: %v", d[0], d[1], ids)
		}
	}
}
