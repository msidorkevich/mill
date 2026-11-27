package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/hook"
)

const hookStatusPending = `{"schema":"mill/status@1","state":"pending","url":"https://ci.example/run/1","checks":[{"name":"build","state":"pending","summary":"running"}]}`

const hookStatusSuccess = `{"schema":"mill/status@1","state":"success","checks":[{"name":"build","state":"success","summary":"ok"}]}`

// hookStatusRepo is an approved task g1/a published to a bare remote. With configured, the
// status hook keeps its stdin in <dir>/status.json, prints <dir>/status.out, and exits 4 with
// "no status" when that file does not exist.
func hookStatusRepo(t *testing.T, configured bool) (root, dir string) {
	t.Helper()
	dir = t.TempDir()
	extra := "hook_timeout = \"30s\"\n"
	if configured {
		script := `cat > '` + dir + `/status.json'; if [ -f '` + dir + `/status.out' ]; then cat '` + dir + `/status.out'; else echo "no status" >&2; exit 4; fi`
		extra += "status = '''" + script + "'''\n"
	}
	root = goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), extra)
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	addRemote(t, root)
	approve(t, root, "g1/a")
	return root, dir
}

func hookStatusWrite(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "status.out"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type hookStatusRecord struct {
	SHA   string `json:"sha"`
	State string `json:"state"`
}

// hookStatusRecords returns the ci-status events of id, oldest first.
func hookStatusRecords(t *testing.T, root, id string) []hookStatusRecord {
	t.Helper()
	evs, err := openStore(t, root).TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []hookStatusRecord
	for _, e := range evs {
		if e.Kind != cistatus.Event {
			continue
		}
		var r hookStatusRecord
		if err := json.Unmarshal(e.Data, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestHookRunStatusPrintsDocumentAndRecordsFirstEvent(t *testing.T) {
	t.Parallel()
	root, dir := hookStatusRepo(t, true)
	hookStatusWrite(t, dir, hookStatusPending)
	before := len(hookStatusRecords(t, root, "g1/a"))

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status")
	if r.code != ExitOK {
		t.Fatalf("hook run --event status: %+v", r)
	}
	var st hook.Status
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		t.Fatalf("stdout is not the status document: %v\n%s", err, r.stdout)
	}
	if st.Schema != hook.StatusSchema || st.State != "pending" || st.URL != "https://ci.example/run/1" || len(st.Checks) != 1 {
		t.Errorf("printed status = %+v", st)
	}
	if !strings.Contains(r.stdout, "\n  \"schema\": \"mill/status@1\"") {
		t.Errorf("stdout is not indented:\n%s", r.stdout)
	}

	br, err := openStore(t, root).GetBranch(context.Background(), "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	if d := hookDoc(t, dir, "status"); d.Event != hook.EventStatus || d.HeadSHA != br.PublishedSHA {
		t.Errorf("status hook document = event %q head %s, want status event at %s", d.Event, d.HeadSHA, br.PublishedSHA)
	}
	recs := hookStatusRecords(t, root, "g1/a")
	if len(recs) != before+1 || recs[len(recs)-1] != (hookStatusRecord{SHA: br.PublishedSHA, State: "pending"}) {
		t.Errorf("ci-status events = %+v, want one pending event at %s", recs, br.PublishedSHA)
	}
}

func TestHookRunStatusRecordsNothingWhenResultUnchanged(t *testing.T) {
	t.Parallel()
	root, dir := hookStatusRepo(t, true)
	hookStatusWrite(t, dir, hookStatusPending)
	if r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status"); r.code != ExitOK {
		t.Fatalf("first run: %+v", r)
	}
	before := len(hookStatusRecords(t, root, "g1/a"))

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"state": "pending"`) {
		t.Fatalf("second run: %+v", r)
	}
	if recs := hookStatusRecords(t, root, "g1/a"); len(recs) != before {
		t.Errorf("unchanged status recorded %d more ci-status events, want none: %+v", len(recs)-before, recs)
	}
}

func TestHookRunStatusRecordsChangedState(t *testing.T) {
	t.Parallel()
	root, dir := hookStatusRepo(t, true)
	hookStatusWrite(t, dir, hookStatusPending)
	if r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status"); r.code != ExitOK {
		t.Fatalf("first run: %+v", r)
	}
	hookStatusWrite(t, dir, hookStatusSuccess)
	before := len(hookStatusRecords(t, root, "g1/a"))

	if r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status"); r.code != ExitOK {
		t.Fatalf("second run: %+v", r)
	}
	recs := hookStatusRecords(t, root, "g1/a")
	if len(recs) != before+1 || recs[len(recs)-1].State != "success" {
		t.Errorf("ci-status events = %+v, want one new success event", recs)
	}
}

func TestHookRunStatusInvalidOutputFailsWithoutRecording(t *testing.T) {
	t.Parallel()
	root, dir := hookStatusRepo(t, true)
	hookStatusWrite(t, dir, "not json")
	before := len(hookStatusRecords(t, root, "g1/a"))

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status")
	if r.code != ExitError || !strings.Contains(r.stderr, "status hook printed no mill/status@1 JSON") {
		t.Fatalf("invalid output: %+v", r)
	}
	if recs := hookStatusRecords(t, root, "g1/a"); len(recs) != before {
		t.Errorf("invalid output recorded ci-status events: %+v", recs)
	}
}

func TestHookRunStatusFailingHookExitsWithError(t *testing.T) {
	t.Parallel()
	root, _ := hookStatusRepo(t, true)

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status")
	if r.code != ExitError || !strings.Contains(r.stderr, "status hook for g1/a failed (exit 4)") || !strings.Contains(r.stderr, "no status") {
		t.Fatalf("failing hook: %+v", r)
	}
	if strings.Contains(r.stdout, "mill/status@1") {
		t.Errorf("failing hook printed a status:\n%s", r.stdout)
	}
}

func TestHookRunStatusWithoutStatusConfiguredNamesKey(t *testing.T) {
	t.Parallel()
	root, _ := hookStatusRepo(t, false)

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "status")
	if r.code != ExitError || !strings.Contains(r.stderr, "hook status is not configured; set status in .mill/config.toml") {
		t.Fatalf("unconfigured status: %+v", r)
	}
}

func TestHookRunStatusUnknownEventListsStatus(t *testing.T) {
	t.Parallel()
	root, _ := hookStatusRepo(t, true)

	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "foo")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "publish|update|status") {
		t.Fatalf("unknown event: %+v", r)
	}
}
