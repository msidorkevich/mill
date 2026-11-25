package keeper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/store"
)

const gateSlotPipelinePoll = time.Millisecond

// gateSlotPipelineWriter is a progress writer safe to read while Combined writes to it.
type gateSlotPipelineWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *gateSlotPipelineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *gateSlotPipelineWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// gateSlotPipelineHold takes the only gate slot as another holder would; it fails the test
// when a slot was left held. Release it with the returned func.
func gateSlotPipelineHold(t *testing.T, s *store.Store) func() {
	t.Helper()
	release, err := s.AcquireGateSlot(context.Background(), store.GateHolder{TaskID: "other", Attempt: 1, PID: os.Getpid()},
		1, gateSlotPipelinePoll, func() { t.Error("the test's own slot had to wait: a slot was left held") })
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGateSlotCombinedCheckWaitsForASlot(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "gate-ran")
	e := combinedSetup(t, "touch '"+marker+"'", []string{"a", "b"}, nil)
	out := &gateSlotPipelineWriter{}
	e.o.Out = out
	e.o.Pipeline = &pipeline.Pipeline{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Home: t.TempDir(),
		MaxParallelGates: e.o.Cfg.MaxParallelGates, GateSlotPoll: gateSlotPipelinePoll}

	release := gateSlotPipelineHold(t, e.o.Store)
	type result struct {
		res CombinedResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := Combined(context.Background(), e.o)
		done <- result{res, err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "waits for a free gate slot") {
		if time.Now().After(deadline) {
			t.Fatalf("the combined check never waited for a gate slot:\n%s", out)
		}
		time.Sleep(gateSlotPipelinePoll)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the combined gate ran while every slot was held (stat: %v)", err)
	}
	release()
	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the combined check did not end:\n%s", out)
	}
	if r.err != nil || !r.res.Ran || !r.res.Passed {
		t.Fatalf("Combined = %+v, %v\n%s", r.res, r.err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the combined gate did not run: %v", err)
	}
	gateSlotPipelineHold(t, e.o.Store)()
	e.assertNoThrowaway(t)
}

func TestGateSlotCombinedCheckReleasesSlotAfterRedProbes(t *testing.T) {
	t.Parallel()
	// No pipeline: the slot comes from the store with max_parallel_gates from the config.
	e := combinedSetup(t, "test ! -f b.txt", []string{"a", "b"}, nil)
	res, err := Combined(context.Background(), e.o)
	if err != nil || res.Passed || res.Culprit == "" || res.Probes < 2 {
		t.Fatalf("Combined = %+v, %v\n%s", res, err, e.out)
	}
	gateSlotPipelineHold(t, e.o.Store)()
}

func TestGateSlotCombinedCheckWaitEndsWhenCancelled(t *testing.T) {
	t.Parallel()
	e := combinedSetup(t, "true", []string{"a", "b"}, nil)
	out := &gateSlotPipelineWriter{}
	e.o.Out = out
	e.o.Pipeline = &pipeline.Pipeline{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Home: t.TempDir(), GateSlotPoll: gateSlotPipelinePoll}
	release := gateSlotPipelineHold(t, e.o.Store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Combined(ctx, e.o)
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "waits for a free gate slot") {
		if time.Now().After(deadline) {
			t.Fatalf("the combined check never waited for a gate slot:\n%s", out)
		}
		time.Sleep(gateSlotPipelinePoll)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "gate slot") {
			t.Fatalf("Combined err = %v, want the cancelled wait for a gate slot", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the combined check kept waiting after cancellation")
	}
	release()
	gateSlotPipelineHold(t, e.o.Store)()
	e.assertNoThrowaway(t)
}
