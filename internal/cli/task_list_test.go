package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// taskListStore opens a store in a temporary directory whose clock reads *clock and which
// treats every claim as alive.
func taskListStore(t *testing.T, clock *time.Time) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mill.db"), store.Options{
		Now:        func() time.Time { return *clock },
		ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// addTask creates task id one minute after the previous one and sets its status.
func addTask(t *testing.T, s *store.Store, clock *time.Time, id, status string) {
	t.Helper()
	ctx := context.Background()
	*clock = clock.Add(time.Minute)
	spec := taskspec.Spec{ID: id, Title: "title " + id, Kind: "implement", Spec: "do it", Acceptance: []string{"done"},
		Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: []string{}}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if status != store.TaskPending {
		if err := s.SetTaskStatus(ctx, id, status, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func listTasks(t *testing.T, s *store.Store, opts taskListOptions, now time.Time) string {
	t.Helper()
	var b strings.Builder
	if err := renderTaskList(context.Background(), &b, s, opts, now); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// rowIDs returns the first field of every table row (header and footer excluded).
func rowIDs(out string) []string {
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		if strings.HasPrefix(l, "…") {
			continue
		}
		ids = append(ids, strings.Fields(l)[0])
	}
	return ids
}

// mixedTasks creates, oldest first: v1 verified, b1 blocked, r1 running, p1 pending,
// f1 failed, c1 verified and claimed by mill run (activity "merging"), v2 verified.
// It returns the time the claim was taken.
func mixedTasks(t *testing.T, s *store.Store, clock *time.Time) time.Time {
	t.Helper()
	for _, tk := range [][2]string{
		{"v1", store.TaskVerified}, {"b1", store.TaskBlocked}, {"r1", store.TaskRunning},
		{"p1", store.TaskPending}, {"f1", store.TaskFailed}, {"c1", store.TaskVerified}, {"v2", store.TaskVerified},
	} {
		addTask(t, s, clock, tk[0], tk[1])
	}
	ctx := context.Background()
	c := store.Claimer{Owner: "o1", PID: 4242, Command: "run", Executable: "mill"}
	if err := s.ClaimTasks(ctx, c, []string{"c1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClaimActivity(ctx, c, "c1", "merging"); err != nil {
		t.Fatal(err)
	}
	return *clock
}

func TestTaskListShowsCurrentWorkThenBlockedFailedThenNewestFirst(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	claimed := mixedTasks(t, s, &clock)

	out := listTasks(t, s, taskListOptions{}, claimed.Add(95*time.Second))
	want := []string{"r1", "c1", "b1", "f1", "v2", "p1", "v1"}
	if got := rowIDs(out); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("rows = %v, want %v:\n%s", got, want, out)
	}
	if !strings.Contains(out, "claimed: merging, held 1m35s") {
		t.Errorf("claimed row lacks activity and held time:\n%s", out)
	}
	if strings.Contains(out, "not shown") {
		t.Errorf("says rows were left out when none were:\n%s", out)
	}
}

func TestTaskListStatusRunningIncludesClaimedTasks(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	claimed := mixedTasks(t, s, &clock)

	out := listTasks(t, s, taskListOptions{Status: store.TaskRunning}, claimed.Add(5*time.Second))
	if got := rowIDs(out); strings.Join(got, " ") != "r1 c1" {
		t.Fatalf("rows = %v, want r1 c1:\n%s", got, out)
	}
	var claimLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "c1 ") {
			claimLine = l
		}
	}
	if !strings.Contains(claimLine, "verified (claimed: merging, held 0m05s)") {
		t.Errorf("claimed row = %q", claimLine)
	}

	// Other status filters keep only that status.
	out = listTasks(t, s, taskListOptions{Status: store.TaskVerified}, claimed)
	if got := rowIDs(out); strings.Join(got, " ") != "c1 v2 v1" {
		t.Fatalf("verified rows = %v:\n%s", got, out)
	}
}

func TestTaskListClaimWithoutActivityShowsCommand(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	addTask(t, s, &clock, "a", store.TaskVerified)
	c := store.Claimer{Owner: "o2", PID: 77, Command: "review"}
	if err := s.ClaimTasks(context.Background(), c, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	out := listTasks(t, s, taskListOptions{}, clock.Add(time.Hour))
	if !strings.Contains(out, "claimed: mill review, held 1h00m00s") {
		t.Errorf("claim without activity:\n%s", out)
	}
}

func TestTaskListShowsTwentyNewestHistoryRowsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	addTask(t, s, &clock, "stuck", store.TaskBlocked)
	for i := 1; i <= 25; i++ {
		addTask(t, s, &clock, fmt.Sprintf("t%02d", i), store.TaskVerified)
	}

	out := listTasks(t, s, taskListOptions{}, clock)
	ids := rowIDs(out)
	if len(ids) != 21 || ids[0] != "stuck" || ids[1] != "t25" || ids[20] != "t06" {
		t.Fatalf("rows = %v:\n%s", ids, out)
	}
	if !strings.HasSuffix(out, "… 5 older task(s) not shown; use --all\n") {
		t.Errorf("missing left-out line:\n%s", out)
	}

	// --all prints every task, oldest first, without the left-out line.
	out = listTasks(t, s, taskListOptions{All: true}, clock)
	ids = rowIDs(out)
	if len(ids) != 26 || ids[0] != "stuck" || ids[1] != "t01" || ids[25] != "t25" {
		t.Fatalf("--all rows = %v:\n%s", ids, out)
	}
	if strings.Contains(out, "not shown") {
		t.Errorf("--all says rows were left out:\n%s", out)
	}
}

func TestTaskListAllKeepsOldestFirstAndMarksClaims(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	claimed := mixedTasks(t, s, &clock)

	out := listTasks(t, s, taskListOptions{All: true}, claimed.Add(time.Minute))
	want := []string{"v1", "b1", "r1", "p1", "f1", "c1", "v2"}
	if got := rowIDs(out); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("rows = %v, want %v:\n%s", got, want, out)
	}
	if !strings.Contains(out, "claimed: merging, held 1m00s") {
		t.Errorf("--all does not mark the claim:\n%s", out)
	}
}

func TestTaskListJSONIsCompleteWithClaims(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := taskListStore(t, &clock)
	claimed := mixedTasks(t, s, &clock)
	for i := 1; i <= 25; i++ {
		addTask(t, s, &clock, fmt.Sprintf("t%02d", i), store.TaskVerified)
	}

	out := listTasks(t, s, taskListOptions{JSON: true}, clock)
	var doc struct {
		Schema string `json:"schema"`
		Tasks  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Claim  *struct {
				PID       int       `json:"pid"`
				Command   string    `json:"command"`
				Activity  string    `json:"activity"`
				ClaimedAt time.Time `json:"claimed_at"`
			} `json:"claim"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Schema != "mill/task-list@1" || len(doc.Tasks) != 32 {
		t.Fatalf("schema %q, %d tasks; want all 32", doc.Schema, len(doc.Tasks))
	}
	if doc.Tasks[0].ID != "v1" || doc.Tasks[31].ID != "t25" {
		t.Errorf("not oldest first: first %s, last %s", doc.Tasks[0].ID, doc.Tasks[31].ID)
	}
	for _, tk := range doc.Tasks {
		if tk.ID == "c1" {
			if c := tk.Claim; c == nil || c.PID != 4242 || c.Command != "run" || c.Activity != "merging" || !c.ClaimedAt.Equal(claimed) {
				t.Errorf("c1 claim = %+v", c)
			}
		} else if tk.Claim != nil {
			t.Errorf("%s has a claim: %+v", tk.ID, tk.Claim)
		}
	}

	// --status running --json includes the claimed task.
	out = listTasks(t, s, taskListOptions{JSON: true, Status: store.TaskRunning}, clock)
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tasks) != 2 || doc.Tasks[0].ID != "r1" || doc.Tasks[1].ID != "c1" {
		t.Errorf("running --json = %+v", doc.Tasks)
	}
}

func TestTaskListAcceptsAllFlag(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	r := run(t, root, nil, "task", "list", "--all")
	if r.code != ExitOK || strings.TrimSpace(r.stdout) != "no tasks" {
		t.Fatalf("task list --all: %+v", r)
	}
}
