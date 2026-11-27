package cistatus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
	"github.com/msidorkevich/mill/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.DisableGitMaintenance(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

const interval = 2 * time.Minute

type ciEnv struct {
	t       *testing.T
	root    string
	scripts string // directory of scripted hook output, one <goal>_<task>.json per task
	calls   string // file the hook appends each task id it was run for to
	s       *store.Store
	cfg     config.Config
	now     time.Time
	out     bytes.Buffer
	errOut  bytes.Buffer
}

func ciGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ciCommit(t *testing.T, dir, name, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ciGit(t, dir, "add", name)
	ciGit(t, dir, "commit", "-q", "-m", msg)
	return ciGit(t, dir, "rev-parse", "HEAD")
}

// ciSetup makes a repo with a bare remote and goal g1 with independent verified tasks a, b
// and c, and a status hook that records the task it runs for and prints the task's
// scripted JSON (failing when there is none).
func ciSetup(t *testing.T) *ciEnv {
	t.Helper()
	ctx := context.Background()
	e := &ciEnv{t: t, root: t.TempDir(), scripts: t.TempDir(), now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	e.calls = filepath.Join(e.scripts, "calls")
	ciGit(t, e.root, "init", "-q", "-b", "main")
	base := ciCommit(t, e.root, "README", "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	ciGit(t, e.root, "init", "-q", "--bare", bare)
	ciGit(t, e.root, "remote", "add", "origin", bare)
	ciGit(t, e.root, "push", "-q", "origin", "main")

	paths := project.PathsAt(e.root)
	if err := os.MkdirAll(paths.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e.s = s
	var specs []taskspec.Spec
	for _, id := range []string{"a", "b", "c"} {
		specs = append(specs, taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"})
	}
	pl := plan.Plan{Goal: "ci", Tasks: specs}
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "ci", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		taskID := "g1/" + id
		wt := filepath.Join(t.TempDir(), id)
		ciGit(t, e.root, "worktree", "add", "-q", "-b", "mill/"+taskID, wt, base)
		head := ciCommit(t, wt, id+".txt", "work on "+id)
		if err := s.SaveBranch(ctx, store.Branch{TaskID: taskID, Name: "mill/" + taskID, Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: head, GateSHA: head, Worktree: wt}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, "task-verdict", "", taskID, map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, taskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = config.Default()
	e.cfg.CheckCmd = "true"
	e.cfg.HookTimeout = config.Duration{Duration: 10 * time.Second}
	e.cfg.StatusInterval = config.Duration{Duration: interval}
	e.cfg.Status = `id=$(sed -n 's/.*"task_id":"\([^"]*\)".*/\1/p'); echo "$id" >> '` + e.calls + `'; ` +
		`f='` + e.scripts + `'/$(echo "$id" | tr / _).json; cat "$f" || { echo "no script for $id" >&2; exit 4; }`
	return e
}

func (e *ciEnv) clock() time.Time { return e.now }

func (e *ciEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

// ciPublish publishes taskID (without the status hook) and returns the published sha.
func (e *ciEnv) ciPublish(taskID string) string {
	e.t.Helper()
	cfg := e.cfg
	cfg.Status = ""
	res, err := publish.Publish(context.Background(), publish.Options{Store: e.s, Cfg: cfg, Paths: project.PathsAt(e.root), TaskID: taskID})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.SHA
}

// ciScript sets the JSON the hook prints for taskID; "" removes it so the hook fails.
func (e *ciEnv) ciScript(taskID, state, url string, checks ...hook.StatusCheck) {
	e.t.Helper()
	path := filepath.Join(e.scripts, strings.ReplaceAll(taskID, "/", "_")+".json")
	if state == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
		return
	}
	if checks == nil {
		checks = []hook.StatusCheck{}
	}
	b, err := json.Marshal(hook.Status{Schema: hook.StatusSchema, State: state, URL: url, Checks: checks})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// ciPoll runs Poll and returns the changes and the task ids the hook ran for, in order.
func (e *ciEnv) ciPoll(goal string) ([]cistatus.Change, []string) {
	e.t.Helper()
	if err := os.Remove(e.calls); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	e.out.Reset()
	e.errOut.Reset()
	changes, err := cistatus.Poll(context.Background(), cistatus.Options{
		Store: e.s, Cfg: e.cfg, Root: e.root, Goal: goal, Out: &e.out, Stderr: &e.errOut, Now: e.clock,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := os.ReadFile(e.calls)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return changes, strings.Fields(string(b))
}

func (e *ciEnv) ciEvents(kind, taskID string) []store.Event {
	e.t.Helper()
	evs, err := e.s.TaskEvents(context.Background(), taskID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestCIStatusPollQueriesPublishedNotLandedTasksInPublishOrder(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	e.ciPublish("g1/c")
	e.ciPublish("g1/b")
	e.ciPublish("g1/a")
	if err := e.s.SetTaskStatus(context.Background(), "g1/b", store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1/a", "g1/b", "g1/c"} {
		e.ciScript(id, "pending", "")
	}
	_, calls := e.ciPoll("")
	if want := []string{"g1/c", "g1/a"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("hook ran for %v, want %v", calls, want)
	}
	if _, calls := e.ciPoll("g2"); len(calls) != 0 {
		t.Errorf("hook ran for %v with another goal selected, want none", calls)
	}
}

func TestCIStatusFinalResultIsNotQueriedUntilIntervalPasses(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	sha := e.ciPublish("g1/a")
	e.ciScript("g1/a", "success", "https://example.test/pr/1")
	changes, calls := e.ciPoll("")
	if len(calls) != 1 || len(changes) != 1 {
		t.Fatalf("first poll: calls %v, changes %+v", calls, changes)
	}
	if got, want := e.out.String(), "g1/a: CI success at "+sha[:8]+" https://example.test/pr/1\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}

	e.advance(interval - time.Second)
	if _, calls := e.ciPoll(""); len(calls) != 0 {
		t.Errorf("hook ran for %v before status_interval passed", calls)
	}
	e.advance(time.Second)
	changes, calls = e.ciPoll("")
	if len(calls) != 1 {
		t.Errorf("hook ran for %v after status_interval, want g1/a", calls)
	}
	if len(changes) != 0 || e.out.Len() != 0 || len(e.ciEvents(cistatus.Event, "g1/a")) != 1 {
		t.Errorf("unchanged result: changes %+v, out %q, %d events", changes, e.out.String(), len(e.ciEvents(cistatus.Event, "g1/a")))
	}
	// The unchanged query counts: the next one waits a full interval again.
	e.advance(interval - time.Second)
	if _, calls := e.ciPoll(""); len(calls) != 0 {
		t.Errorf("hook ran for %v within status_interval of the last unchanged query", calls)
	}
	e.advance(time.Second)
	if _, calls := e.ciPoll(""); len(calls) != 1 {
		t.Errorf("hook ran for %v, want g1/a once the interval passed again", calls)
	}
}

func TestCIStatusPendingResultIsQueriedOnNextPoll(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	e.ciPublish("g1/a")
	e.ciScript("g1/a", "pending", "")
	e.ciPoll("")
	changes, calls := e.ciPoll("")
	if len(calls) != 1 {
		t.Errorf("hook ran for %v on the next poll of a pending task, want g1/a", calls)
	}
	if len(changes) != 0 || e.out.Len() != 0 || len(e.ciEvents(cistatus.Event, "g1/a")) != 1 {
		t.Errorf("unchanged pending: changes %+v, out %q", changes, e.out.String())
	}
}

func TestCIStatusChangeAppendsOneEventAndPrintsFailingChecks(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	sha := e.ciPublish("g1/a")
	e.ciScript("g1/a", "pending", "")
	e.ciPoll("")
	if got, want := e.out.String(), "g1/a: CI pending at "+sha[:8]+"\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}

	checks := []hook.StatusCheck{
		{Name: "unit", State: "failure", Summary: "2 failed"},
		{Name: "lint", State: "success"},
		{Name: "e2e", State: "failure"},
	}
	e.ciScript("g1/a", "failure", "https://example.test/pr/1", checks...)
	changes, _ := e.ciPoll("")
	want := []cistatus.Change{{TaskID: "g1/a", SHA: sha, State: "failure", URL: "https://example.test/pr/1", Checks: checks, Previous: "pending"}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v\nwant      %+v", changes, want)
	}
	if got, want := e.out.String(), "g1/a: CI failure at "+sha[:8]+": unit, e2e https://example.test/pr/1\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	evs := e.ciEvents(cistatus.Event, "g1/a")
	if len(evs) != 2 {
		t.Fatalf("got %d %s events, want 2", len(evs), cistatus.Event)
	}
	rec, found, err := cistatus.Latest(context.Background(), e.s, "g1/a")
	if err != nil || !found {
		t.Fatalf("Latest = %v, %v", found, err)
	}
	if rec.Seq != evs[1].Seq || rec.SHA != sha || rec.State != "failure" || rec.URL != "https://example.test/pr/1" || !reflect.DeepEqual(rec.Checks, checks) {
		t.Errorf("record = %+v", rec)
	}
	if got := cistatus.FailingChecks(rec); len(got) != 2 || got[0].Name != "unit" || got[1].Name != "e2e" {
		t.Errorf("FailingChecks = %+v", got)
	}
	var m map[string]any
	if err := json.Unmarshal(evs[1].Data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sha", "state", "url", "checks"} {
		if _, ok := m[k]; !ok {
			t.Errorf("event data lacks %q: %v", k, m)
		}
	}

	// Same state at the same head: nothing is appended or printed, even after the interval.
	e.advance(interval)
	changes, calls := e.ciPoll("")
	if len(calls) != 1 || len(changes) != 0 || e.out.Len() != 0 || len(e.ciEvents(cistatus.Event, "g1/a")) != 2 {
		t.Errorf("unchanged: calls %v, changes %+v, out %q", calls, changes, e.out.String())
	}
}

func TestCIStatusRecordOfOlderHeadIsStaleAndDue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := ciSetup(t)
	old := e.ciPublish("g1/a")
	e.ciScript("g1/a", "success", "")
	e.ciPoll("")

	br, err := e.s.GetBranch(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	br.PublishedSHA = strings.Repeat("f", 40)
	if err := e.s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	rec, found, err := cistatus.Latest(ctx, e.s, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cistatus.Display(rec, found, br.PublishedSHA), "stale ("+old[:8]+")"; got != want {
		t.Errorf("Display = %q, want %q", got, want)
	}
	if !cistatus.Due(rec, found, br.PublishedSHA, e.now, interval) {
		t.Error("Due = false for a record of an older head")
	}
}

func TestCIStatusHookFailureIsReportedAndPollGoesOn(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	e.ciPublish("g1/a")
	e.ciPublish("g1/b")
	e.ciScript("g1/b", "success", "")

	changes, calls := e.ciPoll("")
	if want := []string{"g1/a", "g1/b"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("hook ran for %v, want %v", calls, want)
	}
	if !strings.Contains(e.errOut.String(), "status hook for g1/a failed (exit 4)") || !strings.Contains(e.errOut.String(), "no script for g1/a") {
		t.Errorf("stderr = %q", e.errOut.String())
	}
	if len(changes) != 1 || changes[0].TaskID != "g1/b" {
		t.Errorf("changes = %+v, want one for g1/b", changes)
	}
	runs := e.ciEvents(hook.RunEvent, "g1/a")
	if len(runs) != 1 {
		t.Fatalf("got %d %s events for g1/a, want 1", len(runs), hook.RunEvent)
	}
	var d struct {
		Event    string `json:"event"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(runs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Event != "status" || d.ExitCode != 4 {
		t.Errorf("hook-run data = %+v", d)
	}
	if len(e.ciEvents(cistatus.Event, "g1/a")) != 0 {
		t.Error("a failed hook recorded a ci-status event")
	}
	task, err := e.s.GetTask(context.Background(), "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskPublished {
		t.Errorf("task status = %s after a hook failure, want published", task.Status)
	}
}

func TestCIStatusUnconfiguredHookDoesNothing(t *testing.T) {
	t.Parallel()
	e := ciSetup(t)
	e.ciPublish("g1/a")
	e.cfg.Status = ""
	changes, calls := e.ciPoll("")
	if changes != nil || len(calls) != 0 || len(e.ciEvents(hook.RunEvent, "g1/a")) != 0 {
		t.Errorf("changes %+v, calls %v with no status hook", changes, calls)
	}
}

func TestCIStatusDisplayAndDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	head := "0123456789abcdef0123456789abcdef01234567"
	old := "fedcba9876543210fedcba9876543210fedcba98"
	if got := cistatus.Display(cistatus.Record{}, false, head); got != "-" {
		t.Errorf("Display with no record = %q, want -", got)
	}
	rec := cistatus.Record{At: now, SHA: head, State: "success", LastQuery: now}
	if got := cistatus.Display(rec, true, head); got != "success" {
		t.Errorf("Display of the current head = %q, want success", got)
	}
	if got := cistatus.Display(rec, true, old); got != "stale (01234567)" {
		t.Errorf("Display of an older head = %q, want stale (01234567)", got)
	}
	for name, c := range map[string]struct {
		rec   cistatus.Record
		found bool
		sha   string
		now   time.Time
		want  bool
	}{
		"nothing recorded":     {cistatus.Record{}, false, head, now, true},
		"other head":           {rec, true, old, now, true},
		"pending":              {cistatus.Record{At: now, SHA: head, State: "pending", LastQuery: now}, true, head, now, true},
		"none":                 {cistatus.Record{At: now, SHA: head, State: "none", LastQuery: now}, true, head, now, true},
		"final within":         {rec, true, head, now.Add(interval - time.Second), false},
		"final after":          {rec, true, head, now.Add(interval), true},
		"recent unchanged run": {cistatus.Record{At: now.Add(-time.Hour), SHA: head, State: "failure", LastQuery: now}, true, head, now.Add(time.Minute), false},
	} {
		if got := cistatus.Due(c.rec, c.found, c.sha, c.now, interval); got != c.want {
			t.Errorf("%s: Due = %v, want %v", name, got, c.want)
		}
	}
}
