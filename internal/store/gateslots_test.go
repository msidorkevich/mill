package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const gateSlotPoll = time.Millisecond

// gateSlotStore opens a store at path whose liveness check reports the pids in dead as
// dead and every other pid as alive. dead may be changed while the test runs.
func gateSlotStore(t *testing.T, path string, dead *sync.Map) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, Options{
		Now: func() time.Time { return fixed },
		ClaimAlive: func(c TaskClaim) (bool, error) {
			if dead == nil {
				return true, nil
			}
			_, d := dead.Load(c.PID)
			return !d, nil
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func gateSlotPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mill.db")
}

func gateSlotHolder(n int) GateHolder {
	return GateHolder{TaskID: fmt.Sprintf("t%d", n), Attempt: 1, PID: 1000 + n}
}

func gateSlotAcquire(t *testing.T, s *Store, h GateHolder, max int, onWait func()) func() error {
	t.Helper()
	release, err := s.AcquireGateSlot(context.Background(), h, max, gateSlotPoll, onWait)
	if err != nil {
		t.Fatalf("AcquireGateSlot(%s): %v", h.TaskID, err)
	}
	return release
}

func gateSlotCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM gate_slots`).Scan(&n); err != nil {
		t.Fatalf("count gate slots: %v", err)
	}
	return n
}

func TestGateSlotSecondAcquirerWaitsUntilFirstReleasesWithMaxOne(t *testing.T) {
	path := gateSlotPath(t)
	s1 := gateSlotStore(t, path, nil)
	s2 := gateSlotStore(t, path, nil)
	release1 := gateSlotAcquire(t, s1, gateSlotHolder(1), 1, func() { t.Error("first acquirer waited") })

	waiting := make(chan struct{})
	type result struct {
		release func() error
		err     error
	}
	done := make(chan result, 1)
	go func() {
		r, err := s2.AcquireGateSlot(context.Background(), gateSlotHolder(2), 1, gateSlotPoll, func() { close(waiting) })
		done <- result{r, err}
	}()

	select {
	case <-waiting:
	case r := <-done:
		t.Fatalf("second acquirer got a slot while the first held it (err %v)", r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("second acquirer neither waited nor acquired")
	}
	select {
	case r := <-done:
		t.Fatalf("second acquirer returned before release (err %v)", r.err)
	default:
	}
	if err := release1(); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("second AcquireGateSlot: %v", r.err)
		}
		if err := r.release(); err != nil {
			t.Fatalf("second release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second acquirer did not get the slot after the first released it")
	}
	if n := gateSlotCount(t, s1); n != 0 {
		t.Fatalf("gate slots after both released = %d, want 0", n)
	}
}

func TestGateSlotMaxTwoLetsBothAcquireWithoutWaiting(t *testing.T) {
	s := gateSlotStore(t, gateSlotPath(t), nil)
	waited := func() { t.Error("acquirer waited with a free slot") }
	r1 := gateSlotAcquire(t, s, gateSlotHolder(1), 2, waited)
	r2 := gateSlotAcquire(t, s, gateSlotHolder(2), 2, waited)
	if n := gateSlotCount(t, s); n != 2 {
		t.Fatalf("gate slots = %d, want 2", n)
	}
	for _, r := range []func() error{r1, r2} {
		if err := r(); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	if n := gateSlotCount(t, s); n != 0 {
		t.Fatalf("gate slots after release = %d, want 0", n)
	}
}

func TestGateSlotHeldByDeadPIDIsReclaimed(t *testing.T) {
	dead := &sync.Map{}
	s := gateSlotStore(t, gateSlotPath(t), dead)
	gateSlotAcquire(t, s, gateSlotHolder(1), 1, nil) // never released: its process dies
	dead.Store(gateSlotHolder(1).PID, true)

	release := gateSlotAcquire(t, s, gateSlotHolder(2), 1, func() { t.Error("waited on a slot held by a dead pid") })
	var pid int
	if err := s.DB().QueryRow(`SELECT pid FROM gate_slots`).Scan(&pid); err != nil {
		t.Fatalf("read gate slot: %v", err)
	}
	if pid != gateSlotHolder(2).PID {
		t.Fatalf("slot holder pid = %d, want %d", pid, gateSlotHolder(2).PID)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestGateSlotWaitStopsWhenContextIsCancelled(t *testing.T) {
	s := gateSlotStore(t, gateSlotPath(t), nil)
	release := gateSlotAcquire(t, s, gateSlotHolder(1), 1, nil)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	done := make(chan error, 1)
	go func() {
		r, err := s.AcquireGateSlot(ctx, gateSlotHolder(2), 1, gateSlotPoll, func() { waits++; cancel() })
		if r != nil {
			err = errors.New("got a release func while the only slot was held")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AcquireGateSlot kept waiting after ctx was cancelled")
	}
	if waits != 1 {
		t.Fatalf("onWait calls = %d, want 1", waits)
	}
	if n := gateSlotCount(t, s); n != 1 {
		t.Fatalf("gate slots = %d, want 1 (only the holder's)", n)
	}
}

func TestGateSlotOnWaitCalledOnceOnlyWhenWaiting(t *testing.T) {
	s := gateSlotStore(t, gateSlotPath(t), nil)
	var waits atomic.Int32
	onWait := func() { waits.Add(1) }

	release1 := gateSlotAcquire(t, s, gateSlotHolder(1), 1, onWait)
	if n := waits.Load(); n != 0 {
		t.Fatalf("onWait calls without waiting = %d, want 0", n)
	}

	done := make(chan error, 1)
	go func() {
		r, err := s.AcquireGateSlot(context.Background(), gateSlotHolder(2), 1, gateSlotPoll, onWait)
		if err == nil {
			err = r()
		}
		done <- err
	}()
	// Let the waiter poll many times before freeing the slot.
	deadline := time.Now().Add(10 * time.Second)
	for waits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("second acquirer never called onWait")
		}
		time.Sleep(gateSlotPoll)
	}
	time.Sleep(20 * gateSlotPoll)
	if err := release1(); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second acquirer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second acquirer did not get the slot")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("onWait calls = %d, want 1", n)
	}
}

func TestGateSlotConcurrentAcquirersNeverExceedMax(t *testing.T) {
	const (
		max       = 2
		acquirers = 8
		rounds    = 5
	)
	path := gateSlotPath(t)
	stores := make([]*Store, acquirers)
	for i := range stores {
		stores[i] = gateSlotStore(t, path, nil)
	}
	var inGate, peak atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, acquirers*rounds)
	for i := range acquirers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				release, err := stores[i].AcquireGateSlot(context.Background(), gateSlotHolder(i), max, gateSlotPoll, nil)
				if err != nil {
					errs <- err
					return
				}
				n := inGate.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(gateSlotPoll)
				inGate.Add(-1)
				if err := release(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("acquirer: %v", err)
	}
	if p := peak.Load(); p > max {
		t.Fatalf("peak gates in progress = %d, want <= %d", p, max)
	}
	if n := gateSlotCount(t, stores[0]); n != 0 {
		t.Fatalf("gate slots after all released = %d, want 0", n)
	}
}
