package syncer

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

// claimActivitySync runs Sync in the background and returns its outcome on a channel.
func claimActivitySync(o Options) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := Sync(context.Background(), o)
		done <- err
	}()
	return done
}

// claimOf reads the live claim on task id, as `mill watch` would. The test binary does not
// run a mill command, so the observer counts every claim's process as alive.
func claimOf(t *testing.T, o Options, id string) store.TaskClaim {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, o.Paths.DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
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

func TestSyncClaimActivityShowsGateWhileTheGateRuns(t *testing.T) {
	t.Parallel()
	o := stackedGoal(t, false)
	l := newClaimActivityLatch(t)
	o.Cfg.CheckCmd = l.script()
	done := claimActivitySync(o)

	l.wait(t)
	c := claimOf(t, o, "g1/b")
	l.open(t)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.Command != "sync" || c.Activity != "sync: gate" || c.ActivitySince.IsZero() {
		t.Fatalf("claim during the gate = %+v, want sync: gate", c)
	}
}

// pushLatch installs a pre-receive hook in the bare remote that waits on a latch.
func pushLatch(t *testing.T, bare string) claimActivityLatch {
	t.Helper()
	l := newClaimActivityLatch(t)
	hook := filepath.Join(strings.TrimSpace(bare), "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncat >/dev/null\n"+l.script()+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSyncClaimActivityShowsPushWhileRepublishing(t *testing.T) {
	t.Parallel()
	o, bare := publishedStack(t)
	l := pushLatch(t, bare)
	done := claimActivitySync(o)

	l.wait(t)
	c := claimOf(t, o, "g1/b")
	l.open(t)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.Command != "sync" || c.Activity != "sync: push" {
		t.Fatalf("claim during the push = %+v, want sync: push", c)
	}
}

func TestSyncClaimActivityErrorStopsTheSyncNamingTaskAndStep(t *testing.T) {
	t.Parallel()
	o, bare := publishedStack(t)
	o.Claimer = store.NewClaimer("sync")
	l := newClaimActivityLatch(t)
	o.Cfg.CheckCmd = l.script()
	done := claimActivitySync(o)

	// The claim is lost while the gate runs: recording the next step must fail.
	l.wait(t)
	if err := o.Store.ReleaseClaims(context.Background(), o.Claimer, []string{"g1/b"}); err != nil {
		t.Fatal(err)
	}
	refs := git(t, o.Paths.Root, "ls-remote", strings.TrimSpace(bare), "refs/heads/review/g1/b")
	l.open(t)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "g1/b") || !strings.Contains(err.Error(), `"push"`) {
		t.Fatalf("sync error = %v, want one naming g1/b and step push", err)
	}
	if after := git(t, o.Paths.Root, "ls-remote", strings.TrimSpace(bare), "refs/heads/review/g1/b"); after != refs {
		t.Fatalf("review/g1/b moved after the failure: %s -> %s", refs, after)
	}
}

// claimUntil polls the live claim on task id until want reports true, failing the test after
// a deadline. It returns the claim that matched.
func claimUntil(t *testing.T, o Options, id, what string, want func(store.TaskClaim) bool) store.TaskClaim {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, o.Paths.DB, store.Options{ClaimAlive: func(store.TaskClaim) (bool, error) { return true, nil }})
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
			t.Fatalf("the claim on %s never showed %s; claims = %+v", id, what, claims)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSyncClaimActivityShowsWaitingForGateUntilItHoldsASlot(t *testing.T) {
	t.Parallel()
	o := stackedGoal(t, false)
	l := newClaimActivityLatch(t)
	o.Cfg.CheckCmd = l.script()
	o.MaxParallelGates, o.GateSlotPoll = 1, time.Millisecond
	release, err := o.Store.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "other", Attempt: 1, PID: os.Getpid()},
		1, time.Millisecond, func() { t.Error("the test's own slot had to wait: a slot was left held") })
	if err != nil {
		t.Fatal(err)
	}
	done := claimActivitySync(o)

	// The task is in phase gate-wait only while its gate waits for a slot.
	c := claimUntil(t, o, "g1/b", "the wait for a gate slot", func(store.TaskClaim) bool {
		tk, err := o.Store.GetTask(context.Background(), "g1/b")
		return err == nil && tk.Phase == store.PhaseGateWait
	})
	if c.Command != "sync" || c.Activity != "sync: waiting for gate" {
		t.Fatalf("claim while waiting for a gate slot = %+v, want sync: waiting for gate", c)
	}
	if _, err := os.Stat(l.started); !os.IsNotExist(err) {
		t.Fatalf("the gate ran while every slot was held (stat: %v)", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	l.wait(t)
	c = claimOf(t, o, "g1/b")
	l.open(t)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.Activity != "sync: gate" {
		t.Fatalf("claim while holding the gate slot = %+v, want sync: gate", c)
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

func TestSyncGateNameCarriesTaskAndCommand(t *testing.T) {
	t.Parallel()
	o := stackedGoal(t, false)
	h := &nameHost{}
	o.Host = h
	if _, err := Sync(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if want := []string{"g1/b sync gate 1"}; fmt.Sprint(h.names) != fmt.Sprint(want) {
		t.Fatalf("gate names = %q, want %q", h.names, want)
	}
}
