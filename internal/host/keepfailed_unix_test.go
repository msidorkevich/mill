//go:build unix

package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// keepFailedTimer is an injected AfterFunc that captures the delays and callbacks.
type keepFailedTimer struct {
	delays []time.Duration
	funcs  []func()
}

func (k *keepFailedTimer) after(d time.Duration, f func()) {
	k.delays = append(k.delays, d)
	k.funcs = append(k.funcs, f)
}

// keepFailedStart starts script under a fake herdr that keeps failed workers for keep.
func keepFailedStart(t *testing.T, keep time.Duration, script string) (hh *Herdr, h Handle, timer *keepFailedTimer, log, logs string) {
	t.Helper()
	hh, log, _ = fakeHerdr(t, "")
	timer = &keepFailedTimer{}
	hh.Poll = time.Millisecond
	hh.KeepFailed, hh.AfterFunc = keep, timer.after
	logs = t.TempDir()
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", script},
		Env:        []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(logs, "out"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return hh, h, timer, log, logs
}

// keepFailedState reports whether herdr was asked to close the tab and how many tab files
// remain in logs.
func keepFailedState(t *testing.T, log, logs string) (closed bool, tabFiles int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(logs, "herdr-*.tab"))
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(herdrLog(t, log), "tab close w8:t2"), len(files)
}

func keepFailedWantKept(t *testing.T, timer *keepFailedTimer, log, logs string) {
	t.Helper()
	if closed, files := keepFailedState(t, log, logs); closed || files != 1 {
		t.Fatalf("after Wait: closed = %v, tab files = %d, want the tab open and recorded", closed, files)
	}
	if len(timer.delays) != 1 || timer.delays[0] != 5*time.Second {
		t.Fatalf("timer delays = %v, want [5s]", timer.delays)
	}
}

func keepFailedWantClosed(t *testing.T, log, logs string) {
	t.Helper()
	if closed, files := keepFailedState(t, log, logs); !closed || files != 0 {
		t.Fatalf("closed = %v, tab files = %d, want the tab closed and its file removed", closed, files)
	}
}

func TestKeepFailedLeavesFailedWorkersTabOpenUntilTimerFires(t *testing.T) {
	t.Parallel()
	_, h, timer, log, logs := keepFailedStart(t, 5*time.Second, "exit 3")
	if ex := waitHerdr(t, h); ex.Code != 3 {
		t.Fatalf("exit = %+v, want code 3", ex)
	}
	keepFailedWantKept(t, timer, log, logs)
	timer.funcs[0]()
	keepFailedWantClosed(t, log, logs)
}

func TestKeepFailedLeavesLostAttemptsTabOpenUntilTimerFires(t *testing.T) {
	t.Parallel()
	_, h, timer, log, logs := keepFailedStart(t, 5*time.Second, "sleep 30")
	if err := syscall.Kill(h.(*herdrHandle).wrapperPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ex, err, _ := waitHerdrResult(t, h)
	if err == nil || ex.Code != -1 {
		t.Fatalf("Wait = %+v, %v, want a lost attempt", ex, err)
	}
	keepFailedWantKept(t, timer, log, logs)
	timer.funcs[0]()
	keepFailedWantClosed(t, log, logs)
}

func TestKeepFailedClosesAtOnceOnSuccessOrWhenDisabled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		keep   time.Duration
		script string
		code   int
	}{
		{"success", 5 * time.Second, "exit 0", 0},
		{"disabled", 0, "exit 3", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, h, timer, log, logs := keepFailedStart(t, tc.keep, tc.script)
			if ex := waitHerdr(t, h); ex.Code != tc.code {
				t.Fatalf("exit = %+v, want code %d", ex, tc.code)
			}
			keepFailedWantClosed(t, log, logs)
			if len(timer.funcs) != 0 {
				t.Fatalf("timer delays = %v, want none", timer.delays)
			}
		})
	}
}

func TestKeepFailedClosesKilledWorkersAtOnce(t *testing.T) {
	t.Parallel()
	_, h, timer, log, logs := keepFailedStart(t, 5*time.Second, "sleep 30")
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	keepFailedWantClosed(t, log, logs)
	if ex := waitHerdr(t, h); !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	if len(timer.funcs) != 0 {
		t.Fatalf("timer delays = %v, want none", timer.delays)
	}
}

func TestKeepFailedReportsDelayedCloseError(t *testing.T) {
	t.Parallel()
	hh, _, _ := fakeHerdr(t, "")
	timer := &keepFailedTimer{}
	var sink bytes.Buffer
	hh.Poll = time.Millisecond
	hh.KeepFailed, hh.AfterFunc, hh.Errors = 5*time.Second, timer.after, &sink
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", "exit 3"},
		Env:        []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(t.TempDir(), "out"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHerdr(t, h)
	// herdr disappears before the timer fires, so the close fails.
	if err := os.Remove(hh.Bin); err != nil {
		t.Fatal(err)
	}
	timer.funcs[0]()
	for _, want := range []string{"close herdr tab w8:t2, kept open", "close herdr tab w8:t2 by hand"} {
		if !strings.Contains(sink.String(), want) {
			t.Fatalf("error sink = %q, want it to contain %q", sink.String(), want)
		}
	}
}
