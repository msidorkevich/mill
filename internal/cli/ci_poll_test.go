package cli

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
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// ciPollHook is a scripted status hook: it appends the task it runs for to calls and prints
// the task's JSON from <dir>/<goal>_<task>.json, failing (exit 4) when there is none.
type ciPollHook struct {
	dir   string
	calls string
}

// ciPollConfigure writes the status hook into root's config with status_interval interval.
func ciPollConfigure(t *testing.T, root, interval string) *ciPollHook {
	t.Helper()
	h := &ciPollHook{dir: t.TempDir()}
	h.calls = filepath.Join(h.dir, "calls")
	script := `id=$(sed -n 's/.*"task_id":"\([^"]*\)".*/\1/p'); echo "$id" >> '` + h.calls + `'
f='` + h.dir + `'/$(echo "$id" | tr / _).json
cat "$f" || { echo "no script for $id" >&2; exit 4; }
`
	sh := filepath.Join(h.dir, "status.sh")
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := project.PathsAt(root).Config
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	top := "status = \"sh " + sh + "\"\nstatus_interval = \"" + interval + "\"\nhook_timeout = \"10s\"\n"
	if err := os.WriteFile(cfgPath, append([]byte(top), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// ciPollScript sets the JSON the hook prints for taskID.
func (h *ciPollHook) ciPollScript(t *testing.T, taskID, state, url string) {
	t.Helper()
	b, err := json.Marshal(hook.Status{Schema: hook.StatusSchema, State: state, URL: url, Checks: []hook.StatusCheck{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, strings.ReplaceAll(taskID, "/", "_")+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ciPollCalls returns the task ids the hook ran for since the last call, in order.
func (h *ciPollHook) ciPollCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(h.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(h.calls); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// ciPollPublished is keepRepo's independent goal g2 with a and b published and the status
// hook configured.
func ciPollPublished(t *testing.T, interval string) (string, *ciPollHook) {
	t.Helper()
	root := keepRepo(t, "true")
	for _, id := range []string{"g2/a", "g2/b"} {
		if r := run(t, root, nil, "review", "approve", id); r.code != ExitOK {
			t.Fatalf("approve %s: %+v", id, r)
		}
	}
	h := ciPollConfigure(t, root, interval)
	return root, h
}

func ciPollEventCount(t *testing.T, s *store.Store, taskID string) int {
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

func ciPollPublishedSHA(t *testing.T, s *store.Store, taskID string) string {
	t.Helper()
	br, err := s.GetBranch(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return br.PublishedSHA
}

func TestCIPollSyncPrintsEachChangeOnceAndRecordsIt(t *testing.T) {
	t.Parallel()
	root, h := ciPollPublished(t, "1h")
	s := openStore(t, root)
	a, b := ciPollPublishedSHA(t, s, "g2/a"), ciPollPublishedSHA(t, s, "g2/b")
	h.ciPollScript(t, "g2/a", "pending", "https://example.test/pr/1")
	h.ciPollScript(t, "g2/b", "success", "")
	h.ciPollCalls(t)

	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	line := "g2/a: CI pending at " + a[:8] + " https://example.test/pr/1\n"
	if strings.Count(r.stdout, line) != 1 {
		t.Errorf("output lacks %q once:\n%s", line, r.stdout)
	}
	if want := "CI:\n  g2/a  pending at " + a[:8] + "\n  g2/b  success at " + b[:8] + "\n"; !strings.Contains(r.stdout, want) {
		t.Errorf("summary lacks %q:\n%s", want, r.stdout)
	}
	if strings.Contains(r.stdout, "Nothing changed.") {
		t.Errorf("sync with CI changes printed Nothing changed.:\n%s", r.stdout)
	}
	if n := ciPollEventCount(t, s, "g2/a"); n != 1 {
		t.Errorf("g2/a has %d %s events, want 1", n, cistatus.Event)
	}

	// The same pending result again: queried, but nothing printed or recorded.
	r = run(t, root, nil, "sync")
	if calls := h.ciPollCalls(t); !reflect.DeepEqual(calls, []string{"g2/a", "g2/b", "g2/a"}) {
		t.Errorf("hook ran for %v over two syncs, want g2/a and g2/b, then only pending g2/a", calls)
	}
	if r.code != ExitOK || strings.Contains(r.stdout, "CI") || !strings.Contains(r.stdout, "Nothing changed.") {
		t.Errorf("second sync: %+v", r)
	}
	if n := ciPollEventCount(t, s, "g2/a"); n != 1 {
		t.Errorf("g2/a has %d %s events after an unchanged result, want 1", n, cistatus.Event)
	}

	h.ciPollScript(t, "g2/a", "success", "https://example.test/pr/1")
	r = run(t, root, nil, "sync")
	if n := strings.Count(r.stdout, ": CI "); r.code != ExitOK || n != 1 || !strings.Contains(r.stdout, "g2/a: CI success at "+a[:8]+" https://example.test/pr/1\n") {
		t.Errorf("third sync: %d CI lines, %+v", n, r)
	}
	if n := ciPollEventCount(t, s, "g2/a"); n != 2 {
		t.Errorf("g2/a has %d %s events after success, want 2", n, cistatus.Event)
	}
}

func TestCIPollSyncJSONIncludesCIChanges(t *testing.T) {
	t.Parallel()
	root, h := ciPollPublished(t, "1h")
	s := openStore(t, root)
	h.ciPollScript(t, "g2/a", "failure", "https://example.test/pr/1")
	h.ciPollScript(t, "g2/b", "pending", "")
	r := run(t, root, nil, "sync", "--json")
	if r.code != ExitOK {
		t.Fatalf("sync --json: %+v", r)
	}
	var doc struct {
		CI []cistatus.Change `json:"ci"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	want := []cistatus.Change{
		{TaskID: "g2/a", SHA: ciPollPublishedSHA(t, s, "g2/a"), State: "failure", URL: "https://example.test/pr/1", Checks: []hook.StatusCheck{}},
		{TaskID: "g2/b", SHA: ciPollPublishedSHA(t, s, "g2/b"), State: "pending", Checks: []hook.StatusCheck{}},
	}
	if !reflect.DeepEqual(doc.CI, want) {
		t.Errorf("ci = %+v\nwant %+v", doc.CI, want)
	}
}

func TestCIPollSyncHookFailureIsReportedAndChangesNoTaskState(t *testing.T) {
	t.Parallel()
	root, h := ciPollPublished(t, "1h")
	s := openStore(t, root)
	h.ciPollScript(t, "g2/b", "success", "")
	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync failed on a status hook failure: %+v", r)
	}
	if !strings.Contains(r.stderr, "status hook for g2/a failed (exit 4)") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if !strings.Contains(r.stdout, "g2/b: CI success") {
		t.Errorf("the other task was not queried:\n%s", r.stdout)
	}
	if n := ciPollEventCount(t, s, "g2/a"); n != 0 {
		t.Errorf("g2/a has %d %s events after a failed hook", n, cistatus.Event)
	}
	for _, id := range []string{"g2/a", "g2/b"} {
		if tk, _ := s.GetTask(context.Background(), id); tk.Status != store.TaskPublished {
			t.Errorf("%s status = %s, want published", id, tk.Status)
		}
	}
}

func TestCIPollKeepQueriesRepublishedTaskAtItsNewHead(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	h := ciPollConfigure(t, root, "1h")
	s := openStore(t, root)
	old := ciPollPublishedSHA(t, s, "g2/b")
	h.ciPollScript(t, "g2/b", "pending", "")
	mergeReview(t, forge(t, bare), "g2/a")

	r := run(t, root, nil, "keep", "--json")
	if r.code != ExitOK {
		t.Fatalf("keep --json: %+v", r)
	}
	var doc struct {
		Keeper struct {
			Sync struct {
				Landed []struct {
					TaskID string `json:"task_id"`
				} `json:"landed"`
				Republished []struct {
					TaskID string `json:"task_id"`
				} `json:"republished"`
				CI []cistatus.Change `json:"ci"`
			} `json:"sync"`
			CI []cistatus.Change `json:"ci"`
		} `json:"keeper"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	k := doc.Keeper
	if len(k.Sync.Landed) != 1 || len(k.Sync.Republished) != 1 || k.Sync.Republished[0].TaskID != "g2/b" {
		t.Fatalf("keep did not land g2/a and re-publish g2/b: %s", r.stdout)
	}
	now := ciPollPublishedSHA(t, s, "g2/b")
	if now == old {
		t.Fatalf("g2/b still published at %s", old)
	}
	if calls := h.ciPollCalls(t); !reflect.DeepEqual(calls, []string{"g2/b"}) {
		t.Errorf("hook ran for %v, want g2/b once", calls)
	}
	want := []cistatus.Change{{TaskID: "g2/b", SHA: now, State: "pending", Checks: []hook.StatusCheck{}}}
	if !reflect.DeepEqual(k.CI, want) || k.Sync.CI == nil || len(k.Sync.CI) != 0 {
		t.Errorf("keeper.ci = %+v, keeper.sync.ci = %+v; want %+v and []", k.CI, k.Sync.CI, want)
	}
	if !strings.Contains(r.stderr, "g2/b: CI pending at "+now[:8]+"\n") {
		t.Errorf("progress lacks the CI line at the new head:\n%s", r.stderr)
	}
}

func TestCIPollKeepWatchQueriesOnEveryPass(t *testing.T) {
	t.Parallel()
	root, h := ciPollPublished(t, "1h")
	s := openStore(t, root)
	a, b := ciPollPublishedSHA(t, s, "g2/a"), ciPollPublishedSHA(t, s, "g2/b")
	h.ciPollScript(t, "g2/a", "success", "")
	h.ciPollScript(t, "g2/b", "pending", "")
	h.ciPollCalls(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var passes [][]string
	wait := func(ctx context.Context, d time.Duration) bool {
		passes = append(passes, h.ciPollCalls(t))
		if len(passes) == 2 {
			cancel()
			return false
		}
		h.ciPollScript(t, "g2/b", "success", "")
		return true
	}
	var out, errb bytes.Buffer
	code := Run(ctx, Env{Stdout: &out, Stderr: &errb, Dir: root, KeepWait: wait}, []string{"keep", "--watch", "--interval", "10s"})
	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, errb.String())
	}
	// Pending g2/b is queried on every pass; final g2/a not again within status_interval.
	if want := [][]string{{"g2/a", "g2/b"}, {"g2/b"}}; !reflect.DeepEqual(passes, want) {
		t.Errorf("hook ran for %v per pass, want %v", passes, want)
	}
	o := out.String()
	for _, want := range []string{
		"g2/a: CI success at " + a[:8] + "\n", "g2/b: CI pending at " + b[:8] + "\n", "g2/b: CI success at " + b[:8] + "\n",
		"CI:\n  g2/a  success at " + a[:8] + "\n  g2/b  pending at " + b[:8] + "\n",
		"CI:\n  g2/b  success at " + b[:8] + "\n",
	} {
		if strings.Count(o, want) != 1 {
			t.Errorf("output lacks %q once:\n%s", want, o)
		}
	}
	if strings.Contains(o, "Nothing to do.") {
		t.Errorf("a pass with CI changes printed Nothing to do.:\n%s", o)
	}
}
