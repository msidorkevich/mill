package keeper

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
)

func TestQueuePublishPrintsHookSuccessLine(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{})
	e.o.Cfg.OnPublish = "echo https://example.test/pr/1"
	if err := e.o.Store.ApproveTask(context.Background(), taskID, 1); err != nil {
		t.Fatal(err)
	}
	e.pass(t)
	want := "hook on_publish for " + taskID + ": https://example.test/pr/1\n"
	if n := strings.Count(e.out.String(), want); n != 1 {
		t.Errorf("output has %d lines %q, want 1:\n%s", n, want, e.out)
	}
}

func TestRepublishPrintsHookSuccessLine(t *testing.T) {
	t.Parallel()
	e := setup(t, "true", runner.Script{})
	if _, err := publish.Publish(context.Background(), publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	e.o.Cfg.OnUpdate = "echo https://example.test/pr/1"
	e.recordResolution(t, e.moveMain(t, "shared.txt", mainSide))
	e.pass(t)
	want := "hook on_update for " + taskID + ": https://example.test/pr/1\n"
	if n := strings.Count(e.out.String(), want); n != 1 {
		t.Errorf("output has %d lines %q, want 1:\n%s", n, want, e.out)
	}
}
