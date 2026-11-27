package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/hook"
)

// appendHookRun records a hook run of taskID the way hook.Runner does.
func appendHookRun(t *testing.T, root, taskID, event string, exitCode int, durationMS int64, timedOut bool, tail string) {
	t.Helper()
	data := map[string]any{
		"event":       event,
		"command":     "true",
		"exit_code":   exitCode,
		"duration_ms": durationMS,
		"output_tail": tail,
		"timed_out":   timedOut,
	}
	if _, err := openStore(t, root).AppendEvent(context.Background(), hook.RunEvent, "", taskID, data); err != nil {
		t.Fatal(err)
	}
}

// lastHookRunAt is the time of the newest hook-run event of taskID.
func lastHookRunAt(t *testing.T, root, taskID string) time.Time {
	t.Helper()
	evs, err := openStore(t, root).TaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	var at time.Time
	for _, e := range evs {
		if e.Kind == hook.RunEvent {
			at = e.At
		}
	}
	if at.IsZero() {
		t.Fatalf("no %s event for %s", hook.RunEvent, taskID)
	}
	return at
}

// hookLineTime formats a run time the way the hook lines print it.
func hookLineTime(at time.Time) string {
	return at.Local().Format("2006-01-02 15:04")
}

// hookLines returns the lines of out that start with "  hook ".
func hookLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  hook ") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestHookShowListsLatestRunPerEvent(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendHookRun(t, root, "g1/a", "publish", 0, 1200, false, "")
	publishAt := lastHookRunAt(t, root, "g1/a")
	appendHookRun(t, root, "g1/a", "update", 0, 300, false, "")
	updateAt := lastHookRunAt(t, root, "g1/a")

	r := run(t, root, nil, "task", "show", "g1/a")
	want := []string{
		"  hook publish: ok (1.2s, " + hookLineTime(publishAt) + ")",
		"  hook update: ok (300ms, " + hookLineTime(updateAt) + ")",
	}
	if r.code != ExitOK || strings.Join(hookLines(r.stdout), "\n") != strings.Join(want, "\n") {
		t.Fatalf("task show: %+v\nwant hook lines:\n%s", r, strings.Join(want, "\n"))
	}

	r = run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || strings.Join(hookLines(r.stdout), "\n") != strings.Join(want, "\n") {
		t.Fatalf("review show: %+v\nwant hook lines:\n%s", r, strings.Join(want, "\n"))
	}
}

func TestHookShowOmitsOlderRunOfSameEvent(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendHookRun(t, root, "g1/a", "publish", 3, 900, false, "old failure")
	appendHookRun(t, root, "g1/a", "publish", 0, 500, false, "")
	at := lastHookRunAt(t, root, "g1/a")

	for _, args := range [][]string{{"task", "show", "g1/a"}, {"review", "show", "g1/a"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		if got, want := hookLines(r.stdout), []string{"  hook publish: ok (500ms, " + hookLineTime(at) + ")"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%v hook lines:\n%s\nwant:\n%s", args, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if strings.Contains(r.stdout, "old failure") || strings.Contains(r.stdout, "exit 3") {
			t.Errorf("%v shows the older publish run:\n%s", args, r.stdout)
		}
	}
}

func TestHookShowFailedRunShowsExitCodeAndFirstTailLine(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendHookRun(t, root, "g1/a", "publish", 2, 1200, false, "lint failed\nsecond line")
	at := lastHookRunAt(t, root, "g1/a")

	r := run(t, root, nil, "task", "show", "g1/a")
	want := "  hook publish: exit 2 (1.2s, " + hookLineTime(at) + "): lint failed\n"
	if r.code != ExitOK || !strings.Contains(r.stdout, want) || strings.Contains(r.stdout, "second line") {
		t.Fatalf("task show: %+v\nwant line %q without the second tail line", r, want)
	}

	r = run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, want) || strings.Contains(r.stdout, "second line") {
		t.Fatalf("review show: %+v\nwant line %q without the second tail line", r, want)
	}
}

func TestHookShowTimedOutRunSaysSo(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendHookRun(t, root, "g1/a", "update", -1, 30000, true, "still waiting")
	at := lastHookRunAt(t, root, "g1/a")

	r := run(t, root, nil, "task", "show", "g1/a")
	want := "  hook update: timed out (30s, " + hookLineTime(at) + "): still waiting\n"
	if r.code != ExitOK || !strings.Contains(r.stdout, want) {
		t.Fatalf("task show: %+v\nwant line %q", r, want)
	}
}

func TestHookShowJSONIncludesHooksObject(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	appendHookRun(t, root, "g1/a", "publish", 4, 1500, false, "boom")
	at := lastHookRunAt(t, root, "g1/a")

	r := run(t, root, nil, "task", "show", "g1/a", "--json")
	if r.code != ExitOK {
		t.Fatalf("task show --json: %+v", r)
	}
	var v struct {
		Hooks map[string]struct {
			ExitCode   int       `json:"exit_code"`
			DurationMS int64     `json:"duration_ms"`
			TimedOut   bool      `json:"timed_out"`
			OutputTail string    `json:"output_tail"`
			At         time.Time `json:"at"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.stdout)
	}
	h, ok := v.Hooks["publish"]
	if !ok {
		t.Fatalf("no hooks.publish in:\n%s", r.stdout)
	}
	if h.ExitCode != 4 || h.DurationMS != 1500 || h.TimedOut || h.OutputTail != "boom" || !h.At.Equal(at) {
		t.Errorf("hooks.publish = %+v, want exit 4, 1500ms, not timed out, tail boom, at %v", h, at)
	}
	if _, ok := v.Hooks["update"]; ok {
		t.Errorf("hooks.update present without an update run:\n%s", r.stdout)
	}
}

func TestHookShowPrintsNothingWithoutHookRuns(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)

	r := run(t, root, nil, "task", "show", "g1/a")
	if r.code != ExitOK || len(hookLines(r.stdout)) != 0 {
		t.Errorf("task show prints hook lines without runs:\n%s", r.stdout)
	}
	r = run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || len(hookLines(r.stdout)) != 0 {
		t.Errorf("review show prints hook lines without runs:\n%s", r.stdout)
	}
	r = run(t, root, nil, "task", "show", "g1/a", "--json")
	var doc map[string]any
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil {
		t.Fatalf("task show --json: %+v", r)
	}
	if _, ok := doc["hooks"]; ok {
		t.Errorf("task show --json has a hooks key without runs:\n%s", r.stdout)
	}
}
