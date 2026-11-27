package keeper

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
)

// hookOrderAfter fails the test unless s has first, then later after it.
func hookOrderAfter(t *testing.T, s, first, later string) {
	t.Helper()
	i := strings.Index(s, first)
	if i < 0 {
		t.Fatalf("output lacks %q:\n%s", first, s)
	}
	j := strings.Index(s, later)
	if j < 0 {
		t.Fatalf("output lacks %q:\n%s", later, s)
	}
	if j < i {
		t.Errorf("%q came before %q:\n%s", later, first, s)
	}
}

// hookOrderRuns counts the hook-run events of the task.
func hookOrderRuns(t *testing.T, e *env) int {
	t.Helper()
	evs, err := e.o.Store.TaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "hook-run" {
			n++
		}
	}
	return n
}

func TestHookLineAfterPublishedKeepRepublish(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{})
	if _, err := publish.Publish(context.Background(), publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	e.o.Cfg.OnUpdate = "echo https://example.test/pr/1"
	e.recordResolution(t, e.moveMain(t, "shared.txt", mainSide))
	e.pass(t)
	hookOrderAfter(t, e.out.String(), taskID+": republished ", "hook on_update for "+taskID+": https://example.test/pr/1\n")
	if n := hookOrderRuns(t, e); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}
}

func TestHookLineAfterPublishedKeepQueue(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{})
	e.o.Cfg.OnPublish = "echo https://example.test/pr/1"
	if err := e.o.Store.ApproveTask(context.Background(), taskID, 1); err != nil {
		t.Fatal(err)
	}
	e.pass(t)
	hookOrderAfter(t, e.out.String(), taskID+": published from the queue as ", "hook on_publish for "+taskID+": https://example.test/pr/1\n")
	if n := hookOrderRuns(t, e); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}
}
