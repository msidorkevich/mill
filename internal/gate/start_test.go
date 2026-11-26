//go:build unix

package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
)

// stuckHerdr is a herdr host whose fake binary creates tabs and accepts `pane run` but never
// runs the wrapper, so the wrapper never writes its pid. Every call is logged to the
// returned file, one per line.
func stuckHerdr(t *testing.T) (*host.Herdr, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "herdr.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2\" in\n" +
		"'tab create') echo '{\"result\":{\"root_pane\":{\"pane_id\":\"w1:p1\"},\"tab\":{\"tab_id\":\"w1:t1\"}}}' ;;\n" +
		"'tab close') echo '{\"result\":{\"type\":\"ok\"}}' ;;\n" +
		"esac\nexit 0\n"
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &host.Herdr{Bin: bin, Poll: time.Millisecond, StartTimeout: 50 * time.Millisecond, Grace: 100 * time.Millisecond}, log
}

func countCalls(t *testing.T, log, prefix string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestStartFailureIsRetriedThenRunsOnExecWithNote(t *testing.T) {
	t.Parallel()
	h, log := stuckHerdr(t)
	o := opts(t, "echo from exec; exit 3")
	o.Host, o.Fallback = h, host.NewExec()
	r := Run(context.Background(), o)
	if got := countCalls(t, log, "pane run"); got != 2 {
		t.Fatalf("herdr started the command %d time(s), want 2 (one retry)", got)
	}
	c := r.Commands[0]
	if c.StartFailed || c.Error != "" || c.Exit != 3 || c.Tail != "from exec" {
		t.Fatalf("command = %+v, want the real result from exec", c)
	}
	if !strings.Contains(c.Note, "herdr could not start it") || !strings.HasSuffix(c.Note, "ran on exec") {
		t.Fatalf("note = %q", c.Note)
	}
	if r.Passed {
		t.Fatal("gate passed with a failing command")
	}
}

func TestStartFailureFallbackPassesGate(t *testing.T) {
	t.Parallel()
	h, _ := stuckHerdr(t)
	o := opts(t, "true")
	o.Host, o.Fallback = h, host.NewExec()
	r := Run(context.Background(), o)
	if !r.Passed || r.Commands[0].Note == "" {
		t.Fatalf("result = %+v, want a pass with a note", r)
	}
}

func TestStartFailureOnEveryHostIsMarkedStartFailed(t *testing.T) {
	t.Parallel()
	h, log := stuckHerdr(t)
	o := opts(t, "true")
	o.Host, o.Fallback = h, host.NewExec()
	o.Dir = "/nonexistent/dir" // exec cannot start it either
	r := Run(context.Background(), o)
	c := r.Commands[0]
	if r.Passed || !c.StartFailed || !strings.Contains(c.Error, "could not run") || !strings.Contains(c.Error, "nor could exec") {
		t.Fatalf("command = %+v", c)
	}
	if got := countCalls(t, log, "pane run"); got != 2 {
		t.Fatalf("herdr started the command %d time(s), want 2", got)
	}
}

func TestStartFailureOnExecIsRetriedWithoutFallback(t *testing.T) {
	t.Parallel()
	o := opts(t, "true")
	o.Dir = "/nonexistent/dir"
	r := Run(context.Background(), o)
	c := r.Commands[0]
	if r.Passed || !c.StartFailed || c.Note != "" || !strings.Contains(c.Error, "could not run") {
		t.Fatalf("command = %+v", c)
	}
}

func TestCommandThatRanAndFailedIsNotStartFailed(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), opts(t, "exit 1"))
	if c := r.Commands[0]; c.StartFailed || c.Note != "" || c.Exit != 1 {
		t.Fatalf("command = %+v", c)
	}
}

func TestStartFailureWithoutFallbackIsMarkedStartFailed(t *testing.T) {
	t.Parallel()
	h, log := stuckHerdr(t)
	o := opts(t, "true")
	o.Host = h
	r := Run(context.Background(), o)
	c := r.Commands[0]
	if r.Passed || !c.StartFailed || c.Note != "" || !strings.Contains(c.Error, "could not run") {
		t.Fatalf("command = %+v", c)
	}
	if got := countCalls(t, log, "pane run"); got != 2 {
		t.Fatalf("herdr started the command %d time(s), want 2", got)
	}
}
