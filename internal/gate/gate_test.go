package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
)

func opts(t *testing.T, cmds ...string) Options {
	return Options{
		Host:    host.NewExec(),
		Dir:     t.TempDir(),
		Cmds:    cmds,
		LogDir:  t.TempDir(),
		Env:     host.WorkerEnv(),
		Timeout: 10 * time.Second,
	}
}

func TestAllPass(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), opts(t, "echo one", "echo two >&2"))
	if !r.Passed || len(r.Commands) != 2 || r.Reason != "" {
		t.Fatalf("result = %+v", r)
	}
	if r.Commands[1].Tail != "two" {
		t.Fatalf("stderr not captured in log: %q", r.Commands[1].Tail)
	}
}

func TestFailureKeepsRunningAndReports(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), opts(t, "echo boom; exit 2", "echo after"))
	if r.Passed {
		t.Fatal("gate passed with a failing command")
	}
	if len(r.Commands) != 2 {
		t.Fatalf("stopped after the failure: %+v", r.Commands)
	}
	if c := r.Commands[0]; c.Exit != 2 || c.Tail != "boom" || c.Passed() {
		t.Fatalf("failed command = %+v", c)
	}
	if !strings.Contains(r.Reason, "echo boom") || strings.Contains(r.Reason, "echo after") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestNoChecksFails(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), opts(t, "", "   "))
	if r.Passed || !strings.Contains(r.Reason, "no checks configured") {
		t.Fatalf("empty gate = %+v", r)
	}
}

func TestTimeoutFails(t *testing.T) {
	t.Parallel()
	o := opts(t, "sleep 5")
	o.Timeout = 200 * time.Millisecond
	r := Run(context.Background(), o)
	if r.Passed || !strings.Contains(r.Commands[0].Error, "timed out") {
		t.Fatalf("timeout = %+v", r)
	}
}

func TestCouldNotRunFails(t *testing.T) {
	t.Parallel()
	o := opts(t, "true")
	o.Dir = "/nonexistent/dir"
	r := Run(context.Background(), o)
	if r.Passed || !strings.Contains(r.Commands[0].Error, "could not run") {
		t.Fatalf("missing dir = %+v", r)
	}
}

func TestRunsInWorktreeWithGivenEnv(t *testing.T) {
	t.Parallel()
	// The environment mill would filter holds a secret; the gate sees only what Env passes.
	o := opts(t, `test -f marker && test -z "$MILL_GATE_SECRET" && test "$MILL_GATE_KEEP" = y`)
	o.Env = host.AllowEnv(append(os.Environ(), "MILL_GATE_SECRET=x", "MILL_GATE_KEEP=y"),
		append(append([]string{}, host.DefaultEnvAllow...), "MILL_GATE_KEEP"))
	if err := os.WriteFile(filepath.Join(o.Dir, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := Run(context.Background(), o); !r.Passed {
		t.Fatalf("gate did not run in Dir with a clean env: %+v", r)
	}
}

func TestLogsAreNeverOverwritten(t *testing.T) {
	t.Parallel()
	o := opts(t, "echo first")
	r1 := Run(context.Background(), o)
	o.Cmds = []string{"echo second"}
	r2 := Run(context.Background(), o)
	if r1.Commands[0].LogPath == r2.Commands[0].LogPath {
		t.Fatal("second run reused the first run's log file")
	}
	b, _ := os.ReadFile(r1.Commands[0].LogPath)
	if string(b) != "first\n" {
		t.Fatalf("first log overwritten: %q", b)
	}
}

func TestTailIsBounded(t *testing.T) {
	t.Parallel()
	o := opts(t, fmt.Sprintf("i=0; while [ $i -lt %d ]; do echo line$i; i=$((i+1)); done", TailLines+50))
	r := Run(context.Background(), o)
	lines := strings.Split(r.Commands[0].Tail, "\n")
	if len(lines) != TailLines || lines[len(lines)-1] != fmt.Sprintf("line%d", TailLines+49) {
		t.Fatalf("tail has %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

// niceness runs a gate that prints its own niceness and returns it.
func niceness(t *testing.T, nice int) int {
	t.Helper()
	o := opts(t, "ps -o nice= -p $$")
	o.Nice = nice
	r := Run(context.Background(), o)
	if !r.Passed {
		t.Fatalf("gate with Nice=%d failed: %+v", nice, r)
	}
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(r.Commands[0].Tail), &n); err != nil {
		t.Fatalf("parse niceness from %q: %v", r.Commands[0].Tail, err)
	}
	return n
}

func TestNiceRunsCommandUnderNice(t *testing.T) {
	t.Parallel()
	base := niceness(t, 0)
	if base > 14 {
		t.Skipf("test process already at niceness %d; cannot observe +5", base)
	}
	if got := niceness(t, 5); got != base+5 {
		t.Fatalf("niceness with Nice=5 = %d, want %d (base %d + 5)", got, base+5, base)
	}
}

func TestNiceZeroRunsCommandUnchanged(t *testing.T) {
	t.Parallel()
	o := opts(t, "echo $0")
	r := Run(context.Background(), o)
	if !r.Passed || r.Commands[0].Tail != "/bin/sh" {
		t.Fatalf("Nice=0 gate = %+v, want /bin/sh run directly", r)
	}
	if got, want := niceness(t, 0), parentNice(t); got != want {
		t.Fatalf("niceness with Nice=0 = %d, want the test process's %d", got, want)
	}
}

// parentNice returns the test process's niceness, read the same way the gate reads its own.
func parentNice(t *testing.T) int {
	t.Helper()
	o := opts(t, fmt.Sprintf("ps -o nice= -p %d", os.Getpid()))
	r := Run(context.Background(), o)
	if !r.Passed {
		t.Fatalf("read test niceness: %+v", r)
	}
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(r.Commands[0].Tail), &n); err != nil {
		t.Fatalf("parse niceness from %q: %v", r.Commands[0].Tail, err)
	}
	return n
}
