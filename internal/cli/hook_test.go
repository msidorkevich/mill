package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/store"
)

// hookRepo is a verified goal with a bare remote whose hooks write their stdin to
// <dir>/<event>.json and print "hook said <event>". A hook exits 3 with "boom" while
// <dir>/fail exists. extra is added to the config as is ("" = both hooks configured).
func hookRepo(t *testing.T, extra string) (root, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `cat > '` + dir + `'/"$MILL_HOOK_EVENT".json; echo "hook said $MILL_HOOK_EVENT"; if [ -f '` + dir + `/fail' ]; then echo boom; exit 3; fi`
	if extra == "" {
		extra = "on_publish = '''" + script + "'''\non_update = '''" + script + "'''\n"
	} else {
		extra = strings.ReplaceAll(extra, "SCRIPT", script)
	}
	root = goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), extra+"hook_timeout = \"30s\"\n")
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	addRemote(t, root)
	return root, dir
}

func approve(t *testing.T, root, id string) {
	t.Helper()
	if r := run(t, root, nil, "review", "approve", id); r.code != ExitOK {
		t.Fatalf("approve %s: %+v", id, r)
	}
}

// hookDoc reads the document the hook received for event.
func hookDoc(t *testing.T, dir, event string) hook.Document {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, event+".json"))
	if err != nil {
		t.Fatalf("the %s hook did not run: %v", event, err)
	}
	var d hook.Document
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("hook stdin is not a document: %v\n%s", err, b)
	}
	return d
}

type hookRunEvent struct {
	Event    string `json:"event"`
	ExitCode int    `json:"exit_code"`
}

func hookRunEvents(t *testing.T, root, id string) []hookRunEvent {
	t.Helper()
	evs, err := openStore(t, root).TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []hookRunEvent
	for _, e := range evs {
		if e.Kind != hook.RunEvent {
			continue
		}
		var h hookRunEvent
		if err := json.Unmarshal(e.Data, &h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

func TestHookRunSendsFreshDocumentAndRecordsEvent(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "")
	approve(t, root, "g1/a")
	before := len(hookRunEvents(t, root, "g1/a"))

	// --event publish explicitly: approve may already have run on_publish, which makes the
	// default update.
	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "publish")
	if r.code != ExitOK {
		t.Fatalf("hook run: %+v", r)
	}
	if !strings.Contains(r.stdout, "hook said publish") || !strings.Contains(r.stdout, "on_publish") {
		t.Errorf("stdout lacks the hook's output tail:\n%s", r.stdout)
	}
	br, err := openStore(t, root).GetBranch(context.Background(), "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	d := hookDoc(t, dir, "publish")
	if d.Schema != hook.Schema || d.Event != "publish" || d.TaskID != "g1/a" || d.GoalID != "g1" ||
		d.ReviewBranch != "review/g1/a" || d.HeadSHA != br.PublishedSHA || d.BaseBranch != "main" || d.PreviousBase != "" {
		t.Errorf("document = %+v", d)
	}
	runs := hookRunEvents(t, root, "g1/a")
	if len(runs) != before+1 || runs[len(runs)-1] != (hookRunEvent{Event: "publish", ExitCode: 0}) {
		t.Errorf("hook-run events = %+v, want one more successful publish run", runs)
	}
}

func TestHookRunDefaultSwitchesToUpdateAfterSuccessfulPublish(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "")
	// A failed publish hook keeps the default at publish; the hook fails from the approval on,
	// so no publish hook has succeeded yet. If approve runs on_publish itself, that run fails
	// and approve may report it; what matters is that the task is published.
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ar := run(t, root, nil, "review", "approve", "g1/a")
	if tk, err := openStore(t, root).GetTask(context.Background(), "g1/a"); err != nil || tk.Status != store.TaskPublished {
		t.Fatalf("approve did not publish g1/a (status %q, err %v): %+v", tk.Status, err, ar)
	}
	before := len(hookRunEvents(t, root, "g1/a"))

	r := run(t, root, nil, "hook", "run", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "hook on_publish for g1/a failed (exit 3)") || !strings.Contains(r.stderr, "boom") {
		t.Fatalf("failing hook: %+v", r)
	}
	if err := os.Remove(filepath.Join(dir, "fail")); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "hook", "run", "g1/a"); r.code != ExitOK || !strings.Contains(r.stdout, "hook said publish") {
		t.Fatalf("second run: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "update.json")); err == nil {
		t.Fatal("on_update ran before a publish hook succeeded")
	}

	// After the successful publish hook the default is update.
	r = run(t, root, nil, "hook", "run", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hook said update") {
		t.Fatalf("third run: %+v", r)
	}
	if d := hookDoc(t, dir, "update"); d.Event != "update" || d.TaskID != "g1/a" {
		t.Errorf("update document = %+v", d)
	}
	// --event overrides the default.
	if r := run(t, root, nil, "hook", "run", "g1/a", "--event", "publish"); r.code != ExitOK || !strings.Contains(r.stdout, "hook said publish") {
		t.Fatalf("--event publish: %+v", r)
	}
	want := []hookRunEvent{{"publish", 3}, {"publish", 0}, {"update", 0}, {"publish", 0}}
	if got := hookRunEvents(t, root, "g1/a")[before:]; !slices.Equal(got, want) {
		t.Errorf("hook-run events = %+v, want %+v", got, want)
	}
}

func TestHookRunRefusesUnpublishedTask(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "")
	r := run(t, root, nil, "hook", "run", "g1/a")
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitError, r)
	}
	for _, want := range []string{"g1/a", "not published", "mill review approve g1/a"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "publish.json")); err == nil {
		t.Error("the hook ran for an unpublished task")
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a"); tk.Status != store.TaskVerified {
		t.Errorf("status = %s, want verified", tk.Status)
	}
}

func TestHookRunUnconfiguredHookFails(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "on_publish = '''SCRIPT'''\n")
	approve(t, root, "g1/a")
	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "update")
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitError, r)
	}
	if !strings.Contains(r.stderr, "hook on_update is not configured; set it in .mill/config.toml") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "update.json")); err == nil {
		t.Error("a hook ran for an unconfigured event")
	}
}

func TestHookRunBadEventIsUsageError(t *testing.T) {
	t.Parallel()
	root, _ := hookRepo(t, "")
	approve(t, root, "g1/a")
	r := run(t, root, nil, "hook", "run", "g1/a", "--event", "merge")
	if r.code != ExitUsage || !strings.Contains(r.stderr, `unknown --event "merge"`) || !strings.Contains(r.stderr, "publish or update") {
		t.Fatalf("bad --event: %+v", r)
	}
	if r := run(t, root, nil, "hook"); r.code != ExitUsage || !strings.Contains(r.stderr, "mill hook run <task>") {
		t.Errorf("no subcommand: %+v", r)
	}
	if r := run(t, root, nil, "hook", "run"); r.code != ExitUsage {
		t.Errorf("no task: %+v", r)
	}
}

func TestHookRunHelpListsHookCommand(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "help")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hook ") || !strings.Contains(r.stdout, "re-run a publish hook for a published task") {
		t.Errorf("help: %+v", r)
	}
}
