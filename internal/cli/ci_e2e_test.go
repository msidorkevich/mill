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

const (
	ciE2ETask = "g1/a"
	ciE2EURL  = "https://example.test/pr/1"
	// ciE2EInterval is status_interval: long enough that it never elapses during the test, so
	// only a new head or a non-final state makes a query due.
	ciE2EInterval = time.Hour
)

// ciE2EOutput is the stdout of keep --watch. It calls onFixer once, when the keeper announces
// the fixer: the only moment the failure is recorded for the head that is still published.
type ciE2EOutput struct {
	bytes.Buffer
	onFixer func()
}

func (o *ciE2EOutput) Write(p []byte) (int, error) {
	if o.onFixer != nil && bytes.Contains(p, []byte("running the fixer")) {
		f := o.onFixer
		o.onFixer = nil
		f()
	}
	return o.Buffer.Write(p)
}

// ciE2ESetup publishes g1/a to a bare remote with the fake runner, configures the scripted
// status hook with ci_fix, and leaves the fake script set up for one fixer run and verdict.
func ciE2ESetup(t *testing.T) (root, bare string, h *ciPollHook) {
	t.Helper()
	root, bare = publishedGoal(t)
	if r := run(t, root, nil, "review", "approve", ciE2ETask); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	h = ciPollConfigure(t, root, ciE2EInterval.String())
	cfgPath := project.PathsAt(root).Config
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append([]byte("ci_fix = true\n"), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every keep pass loads the script afresh, so these are the fixer's first steps.
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(fanInScript(`{"a.txt": "fixed\n"}`)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, bare, h
}

// ciE2EStatus makes the status hook report state with checks for g1/a.
func ciE2EStatus(t *testing.T, h *ciPollHook, state string, checks ...hook.StatusCheck) {
	t.Helper()
	if checks == nil {
		checks = []hook.StatusCheck{}
	}
	b, err := json.Marshal(hook.Status{Schema: hook.StatusSchema, State: state, URL: ciE2EURL, Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, strings.ReplaceAll(ciE2ETask, "/", "_")+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ciE2EColumn is the CI column of g1/a in `mill review`.
func ciE2EColumn(t *testing.T, root string) string {
	t.Helper()
	r := run(t, root, nil, "review")
	if r.code != ExitOK {
		t.Fatalf("review: %+v", r)
	}
	return ciDisplayColumn(t, r.stdout, ciE2ETask)
}

// ciE2EShows asserts the CI block of `mill task show` for g1/a.
func ciE2EShows(t *testing.T, root, state, sha string) {
	t.Helper()
	r := run(t, root, nil, "task", "show", ciE2ETask)
	want := "\nCI:\n  state  " + state + "\n  sha    " + sha[:8] + "\n"
	if r.code != ExitOK || !strings.Contains(r.stdout, want) {
		t.Errorf("task show lacks %q: %+v", want, r)
	}
}

func TestCIEndToEndKeepWatchShowsAndFixesAFailure(t *testing.T) {
	t.Parallel()
	root, bare, h := ciE2ESetup(t)
	s := openStore(t, root)
	old := ciPollPublishedSHA(t, s, ciE2ETask)
	ciE2EStatus(t, h, "pending")
	h.ciPollCalls(t)

	out := &ciE2EOutput{}
	var atFixer string
	out.onFixer = func() { atFixer = ciE2EColumn(t, root) }

	var fixed string // the head published by the fix
	var queries []int
	passes := 0
	wait := func(ctx context.Context, _ time.Duration) bool {
		passes++
		queries = append(queries, len(h.ciPollCalls(t)))
		switch passes {
		case 1: // pending was reported; a non-final state stays due
			if got := ciE2EColumn(t, root); got != "pending" {
				t.Errorf("review CI after pass 1 = %q, want pending", got)
			}
			ciE2EStatus(t, h, "failure",
				hook.StatusCheck{Name: "unit", State: "failure", URL: "https://example.test/checks/unit", Summary: "FAIL TestA"})
		case 2: // failure was fixed and re-published; the new head is due but not queried yet
			fixed = ciPollPublishedSHA(t, s, ciE2ETask)
			if fixed == old {
				t.Fatalf("%s was not re-published after the failure:\n%s", ciE2ETask, out)
			}
			stale := "stale (" + old[:8] + ")"
			ciE2EShows(t, root, stale, old)
			if got := ciE2EColumn(t, root); got != stale {
				t.Errorf("review CI after the re-publish = %q, want %q", got, stale)
			}
			ciE2EStatus(t, h, "success")
		case 3: // success at the new head
			ciE2EShows(t, root, "success", fixed)
			if got := ciE2EColumn(t, root); got != "success" {
				t.Errorf("review CI after pass 3 = %q, want success", got)
			}
		case 4: // success is final at the published head: nothing was due
			return false
		}
		select {
		case <-time.After(time.Millisecond):
			return true
		case <-ctx.Done():
			return false
		}
	}

	// The deadline only ends a watch that stopped calling wait.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var errb bytes.Buffer
	env := Env{Stdout: out, Stderr: &errb, Dir: root, KeepWait: wait}
	code := Run(ctx, env, []string{"keep", "g1", "--watch", "--interval", keepMinInterval.String()})
	if code != ExitOK || passes != 4 {
		t.Fatalf("code = %d after %d passes\nstdout:\n%s\nstderr:\n%s", code, passes, out, errb.String())
	}
	if want := []int{1, 1, 1, 0}; !reflect.DeepEqual(queries, want) {
		t.Errorf("status hook runs per pass = %v, want %v", queries, want)
	}
	if atFixer != "failure" {
		t.Errorf("review CI when the fixer started = %q, want failure", atFixer)
	}

	// The CI lines and the per-pass summaries, in the order printed.
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		summary := strings.HasPrefix(l, "  "+ciE2ETask+"  pending at ") ||
			strings.HasPrefix(l, "  "+ciE2ETask+"  failure at ") || strings.HasPrefix(l, "  "+ciE2ETask+"  success at ")
		if summary || strings.Contains(l, ": CI ") || strings.HasPrefix(l, ciE2ETask+": republished ") {
			lines = append(lines, l)
		}
	}
	republished := ciE2ETask + ": republished "
	for i, l := range lines {
		if strings.HasPrefix(l, republished) {
			lines[i] = republished
		}
	}
	want := []string{
		"g1/a: CI pending at " + old[:8] + " " + ciE2EURL,
		"  g1/a  pending at " + old[:8],
		"g1/a: CI failure at " + old[:8] + ": unit " + ciE2EURL,
		"g1/a: CI failed at " + old[:8] + " (unit); running the fixer",
		republished,
		"  g1/a  failure at " + old[:8],
		"g1/a: CI success at " + fixed[:8] + " " + ciE2EURL,
		"  g1/a  success at " + fixed[:8],
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("CI lines =\n%s\nwant\n%s\noutput:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"), out)
	}

	evs, err := s.TaskEvents(context.Background(), ciE2ETask)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for _, ev := range evs {
		if ev.Kind != cistatus.Event {
			continue
		}
		var d struct{ SHA, State string }
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, d.State+" "+d.SHA)
	}
	if want := []string{"pending " + old, "failure " + old, "success " + fixed}; !reflect.DeepEqual(recorded, want) {
		t.Errorf("%s events = %v, want %v", cistatus.Event, recorded, want)
	}

	// The fix went through the fake implementer and verifier and reached the remote.
	if got := remoteRefs(t, bare)["refs/heads/review/g1/a"]; got != fixed {
		t.Errorf("review branch = %s, want %s", got, fixed)
	}
	if got := gitC(t, bare, "show", fixed+":a.txt"); strings.TrimSpace(got) != "fixed" {
		t.Errorf("a.txt on the review branch = %q, want fixed", got)
	}
	if tk, err := s.GetTask(context.Background(), ciE2ETask); err != nil || tk.Status != store.TaskPublished {
		t.Errorf("%s = %+v, %v; want published", ciE2ETask, tk, err)
	}
}
