package keeper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/testenv"
)

// claimActivityLatch is a gate command or git hook that creates started and waits until
// release exists. The waiting shell runs in its own process group, killed when the test
// ends, and gives up on its own once the latch directory is gone or after a minute.
type claimActivityLatch struct {
	dir, started, release string
	groups                *testenv.Groups
}

func newClaimActivityLatch(t *testing.T) claimActivityLatch {
	dir := t.TempDir()
	return claimActivityLatch{dir: dir, started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release"),
		groups: testenv.NewGroups(t)}
}

func (l claimActivityLatch) script() string {
	return l.groups.Script("touch " + testenv.Quote(l.started) + "; " + testenv.WaitForFileScript(l.dir, "release"))
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

// claimActivityUntil polls the live claim on task id until want reports true, failing the
// test after a deadline. It returns the claim that matched.
func (e *env) claimActivityUntil(t *testing.T, id string, what string, want func(store.TaskClaim) bool) store.TaskClaim {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, e.o.Paths.DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		claims, err := s.ClaimedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := claims[id]; ok && want(c) {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claim on %s never showed %s; claims = %+v\n%s", id, what, claims, e.out)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKeepClaimActivityShowsWaitingForGateUntilItHoldsASlot(t *testing.T) {
	t.Parallel()
	l := newClaimActivityLatch(t)
	e := rebasedByRerere(t, l.script())
	e.o.MaxParallelGates, e.o.GateSlotPoll = 1, gateSlotPipelinePoll
	release := gateSlotPipelineHold(t, e.o.Store)
	done := e.claimActivityPass()

	// The task is in phase gate-wait only while the gate waits for a slot.
	c := e.claimActivityUntil(t, taskID, "the wait for a gate slot", func(store.TaskClaim) bool {
		tk, err := e.o.Store.GetTask(context.Background(), taskID)
		return err == nil && tk.Phase == store.PhaseGateWait
	})
	if c.Command != "keep" || c.Activity != "keep: waiting for gate" {
		t.Fatalf("claim while waiting for a gate slot = %+v, want keep: waiting for gate", c)
	}
	if _, err := os.Stat(l.started); !os.IsNotExist(err) {
		t.Fatalf("the gate ran while every slot was held (stat: %v)", err)
	}
	release()
	l.wait(t)
	c = e.claimOf(t, taskID)
	l.open(t)
	if err := <-done; err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	if c.Activity != "keep: gate" {
		t.Fatalf("claim while holding the gate slot = %+v, want keep: gate", c)
	}
}

// nameHost runs every command on the exec host and records the name it was started with,
// as a herdr host shows it in the tab label.
type nameHost struct {
	mu    sync.Mutex
	names []string
}

func (h *nameHost) Name() string { return "names" }

func (h *nameHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	h.mu.Lock()
	h.names = append(h.names, spec.Name)
	h.mu.Unlock()
	return host.NewExec().Start(ctx, spec)
}

func (h *nameHost) started() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.names...)
}

func TestKeepGateNameCarriesTaskAndCommand(t *testing.T) {
	t.Parallel()
	e := rebasedByRerere(t, "true")
	h := &nameHost{}
	e.o.Host = h
	if _, err := Pass(context.Background(), e.o); err != nil {
		t.Fatalf("Pass: %v\n%s", err, e.out)
	}
	want := []string{taskID + " keeper gate 1"}
	if got := h.started(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gate names = %q, want %q\n%s", got, want, e.out)
	}
}
