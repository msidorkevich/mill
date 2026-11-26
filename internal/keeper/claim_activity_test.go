package keeper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// claimActivityLatch is a gate command or git hook that creates started and waits until
// release exists, polling every few milliseconds.
type claimActivityLatch struct{ started, release string }

func newClaimActivityLatch(t *testing.T) claimActivityLatch {
	dir := t.TempDir()
	return claimActivityLatch{started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release")}
}

func (l claimActivityLatch) script() string {
	return "touch '" + l.started + "'; while [ ! -f '" + l.release + "' ]; do sleep 0.005; done"
}

// wait polls until the latch's holder started, failing the test after a deadline.
func (l claimActivityLatch) wait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(l.started); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the latched step never started")
		}
		time.Sleep(time.Millisecond)
	}
}

func (l claimActivityLatch) open(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(l.release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// claimActivityPass runs a keeper pass in the background and returns its outcome.
func (e *env) claimActivityPass() <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := Pass(context.Background(), e.o)
		done <- err
	}()
	return done
}

// claimOf reads the live claim on task id, as `mill watch` would. The test binary does not
// run a mill command, so the observer counts every claim's process as alive.
func (e *env) claimOf(t *testing.T, id string) store.TaskClaim {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, e.o.Paths.DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	claims, err := s.ClaimedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := claims[id]
	if !ok {
		t.Fatalf("%s is not claimed; claims = %+v", id, claims)
	}
	return c
}

// rebasedByRerere sets the task up so that the pass rebases it with rerere (no agent) and
// then gates it with gate.
func rebasedByRerere(t *testing.T, gate string) *env {
	t.Helper()
	e := setup(t, gate, runner.Script{})
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)
	return e
}

func TestKeepClaimActivityShowsGateWhileTheGateRuns(t *testing.T) {
	t.Parallel()
	l := newClaimActivityLatch(t)
	e := rebasedByRerere(t, l.script())
	done := e.claimActivityPass()

	l.wait(t)
	c := e.claimOf(t, taskID)
	l.open(t)
	if err := <-done; err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if c.Command != "keep" || c.Activity != "keep: gate" || c.ActivitySince.IsZero() {
		t.Fatalf("claim during the gate = %+v, want keep: gate", c)
	}
}

// publishedRebase is rebasedByRerere with the task published before main moved, so that the
// pass re-publishes it after the gate.
func publishedRebase(t *testing.T, gate string) *env {
	t.Helper()
	e := setup(t, "true", runner.Script{})
	if _, err := publish.Publish(context.Background(), publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)
	e.o.Cfg.CheckCmd = gate
	e.o.Pipeline.Cfg.CheckCmd = gate
	return e
}

func TestKeepClaimActivityShowsPublishWhileRepublishing(t *testing.T) {
	t.Parallel()
	e := publishedRebase(t, "true")
	l := newClaimActivityLatch(t)
	hook := filepath.Join(e.bare, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncat >/dev/null\n"+l.script()+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	done := e.claimActivityPass()

	l.wait(t)
	c := e.claimOf(t, taskID)
	l.open(t)
	if err := <-done; err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if c.Command != "keep" || c.Activity != "keep: publish" {
		t.Fatalf("claim during the push = %+v, want keep: publish", c)
	}
}

func TestKeepClaimActivityErrorStopsThePassNamingTaskAndStep(t *testing.T) {
	t.Parallel()
	l := newClaimActivityLatch(t)
	e := publishedRebase(t, l.script())
	e.o.Claimer = store.NewClaimer("keep")
	before := git(t, e.bare, "rev-parse", "refs/heads/"+publish.ReviewRef(taskID))
	done := e.claimActivityPass()

	// The claim is lost while the gate runs: recording the next step must fail.
	l.wait(t)
	if err := e.o.Store.ReleaseClaims(context.Background(), e.o.Claimer, []string{taskID}); err != nil {
		t.Fatal(err)
	}
	l.open(t)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), taskID) || !strings.Contains(err.Error(), `"publish"`) {
		t.Fatalf("Pass error = %v, want one naming %s and step publish\n%s", err, taskID, e.out)
	}
	if after := git(t, e.bare, "rev-parse", "refs/heads/"+publish.ReviewRef(taskID)); after != before {
		t.Fatalf("review branch moved after the failure: %s -> %s", before, after)
	}
}
