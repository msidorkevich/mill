package syncer

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
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/store"
)

const ciPollInterval = 2 * time.Minute

// ciPollEnv is a published stack (g1/b restacked and re-published by the next sync) with a
// status hook that appends the task it runs for to calls and prints the task's scripted JSON
// from scripts/<goal>_<task>.json, failing when there is none. The CI query runs on now.
type ciPollEnv struct {
	o       Options
	scripts string
	calls   string
	now     time.Time
	out     bytes.Buffer
	errOut  bytes.Buffer
}

func ciPollSetup(t *testing.T) *ciPollEnv {
	t.Helper()
	o, _ := publishedStack(t)
	e := &ciPollEnv{o: o, scripts: t.TempDir(), now: time.Unix(0, 0)} // the fixture's store clock
	e.calls = filepath.Join(e.scripts, "calls")
	e.o.Cfg.HookTimeout = config.Duration{Duration: 10 * time.Second}
	e.o.Cfg.StatusInterval = config.Duration{Duration: ciPollInterval}
	e.o.Cfg.Status = `id=$(sed -n 's/.*"task_id":"\([^"]*\)".*/\1/p'); echo "$id" >> '` + e.calls + `'; ` +
		`f='` + e.scripts + `'/$(echo "$id" | tr / _).json; cat "$f" || { echo "no script for $id" >&2; exit 4; }`
	e.o.Now = func() time.Time { return e.now }
	e.o.Out, e.o.Stderr = &e.out, &e.errOut
	return e
}

func (e *ciPollEnv) ciPollScript(t *testing.T, taskID, state, url string) {
	t.Helper()
	b, err := json.Marshal(hook.Status{Schema: hook.StatusSchema, State: state, URL: url, Checks: []hook.StatusCheck{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.scripts, strings.ReplaceAll(taskID, "/", "_")+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ciPollSync runs Sync and returns its result and the task ids the hook ran for, in order.
func (e *ciPollEnv) ciPollSync(t *testing.T) (Result, []string) {
	t.Helper()
	if err := os.Remove(e.calls); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	e.out.Reset()
	e.errOut.Reset()
	res, err := Sync(context.Background(), e.o)
	if err != nil {
		t.Fatalf("sync: %v\nstderr: %s", err, e.errOut.String())
	}
	b, err := os.ReadFile(e.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return res, strings.Fields(string(b))
}

func ciPollEvents(t *testing.T, s *store.Store, taskID string) int {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == cistatus.Event {
			n++
		}
	}
	return n
}

func TestCIPollSyncQueriesRepublishedTaskAtItsNewHead(t *testing.T) {
	t.Parallel()
	e := ciPollSetup(t)
	ctx := context.Background()
	e.ciPollScript(t, "g1/a", "success", "https://example.test/pr/1")
	e.ciPollScript(t, "g1/b", "pending", "")

	res, calls := e.ciPollSync(t)
	if len(res.Republished) != 1 || res.Republished[0].TaskID != "g1/b" {
		t.Fatalf("republished = %+v", res.Republished)
	}
	if want := []string{"g1/a", "g1/b"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("hook ran for %v, want %v", calls, want)
	}
	ab, _ := e.o.Store.GetBranch(ctx, "g1/a")
	bb, _ := e.o.Store.GetBranch(ctx, "g1/b")
	if bb.PublishedSHA != res.Republished[0].SHA {
		t.Fatalf("g1/b published at %s, republished %s", bb.PublishedSHA, res.Republished[0].SHA)
	}
	want := []cistatus.Change{
		{TaskID: "g1/a", SHA: ab.PublishedSHA, State: "success", URL: "https://example.test/pr/1", Checks: []hook.StatusCheck{}},
		{TaskID: "g1/b", SHA: bb.PublishedSHA, State: "pending", Checks: []hook.StatusCheck{}},
	}
	if !reflect.DeepEqual(res.CI, want) {
		t.Errorf("ci = %+v\nwant %+v", res.CI, want)
	}
	line := "g1/b: CI pending at " + bb.PublishedSHA[:8] + "\n"
	if strings.Count(e.out.String(), line) != 1 {
		t.Errorf("output lacks %q once:\n%s", line, e.out.String())
	}
	if strings.Index(e.out.String(), line) < strings.Index(e.out.String(), "g1/b: republished") {
		t.Errorf("the CI line came before the republish:\n%s", e.out.String())
	}
}

func TestCIPollSyncLimitsRequeriesOfFinalStatesByStatusInterval(t *testing.T) {
	t.Parallel()
	e := ciPollSetup(t)
	e.ciPollScript(t, "g1/a", "success", "")
	e.ciPollScript(t, "g1/b", "pending", "")
	if res, _ := e.ciPollSync(t); len(res.CI) != 2 {
		t.Fatalf("first sync ci = %+v", res.CI)
	}

	// Pending is queried on every sync; the final success waits for status_interval.
	e.now = e.now.Add(ciPollInterval - time.Second)
	res, calls := e.ciPollSync(t)
	if want := []string{"g1/b"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("hook ran for %v within status_interval, want %v", calls, want)
	}
	if len(res.CI) != 0 || strings.Contains(e.out.String(), "CI ") {
		t.Errorf("unchanged results: ci %+v, out:\n%s", res.CI, e.out.String())
	}
	e.now = e.now.Add(time.Second)
	if _, calls := e.ciPollSync(t); !reflect.DeepEqual(calls, []string{"g1/a", "g1/b"}) {
		t.Errorf("hook ran for %v once status_interval passed, want g1/a and g1/b", calls)
	}
	if n := ciPollEvents(t, e.o.Store, "g1/a"); n != 1 {
		t.Errorf("g1/a has %d %s events, want 1", n, cistatus.Event)
	}
}

func TestCIPollSyncHookFailureIsReportedAndChangesNothing(t *testing.T) {
	t.Parallel()
	e := ciPollSetup(t)
	ctx := context.Background()
	e.ciPollScript(t, "g1/a", "success", "")
	res, calls := e.ciPollSync(t)
	if want := []string{"g1/a", "g1/b"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("hook ran for %v, want %v", calls, want)
	}
	if !strings.Contains(e.errOut.String(), "status hook for g1/b failed (exit 4)") {
		t.Errorf("stderr = %q", e.errOut.String())
	}
	if len(res.CI) != 1 || res.CI[0].TaskID != "g1/a" {
		t.Errorf("ci = %+v, want one change for g1/a", res.CI)
	}
	if n := ciPollEvents(t, e.o.Store, "g1/b"); n != 0 {
		t.Errorf("g1/b has %d %s events after a failed hook", n, cistatus.Event)
	}
	if tk, _ := e.o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskPublished {
		t.Errorf("g1/b status = %s, want published", tk.Status)
	}
}

func TestCIPollSyncSkipCIStatusRunsNoHook(t *testing.T) {
	t.Parallel()
	e := ciPollSetup(t)
	e.ciPollScript(t, "g1/a", "success", "")
	e.ciPollScript(t, "g1/b", "success", "")
	e.o.SkipCIStatus = true
	res, calls := e.ciPollSync(t)
	if len(calls) != 0 || len(res.CI) != 0 || res.CI == nil {
		t.Errorf("with SkipCIStatus: hook ran for %v, ci = %#v", calls, res.CI)
	}
}
