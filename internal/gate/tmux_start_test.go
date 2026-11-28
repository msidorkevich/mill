//go:build unix

package gate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/testenv"
)

// gateTmux returns a tmux host whose fake tmux runs each new window's command in the
// background through the testenv pgrun helper, and a log file of every tmux call, one per
// line. With startFails, new-window fails the way a tmux server that is gone does, so the
// host cannot start any window. Each open window is a file in dir/windows, removed when its
// command ends, as tmux closes a window whose command has ended.
func gateTmux(t *testing.T, startFails bool) (tm *host.Tmux, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "tmux.log")
	windows := filepath.Join(dir, "windows")
	if err := os.Mkdir(windows, 0o755); err != nil {
		t.Fatal(err)
	}
	counter := testenv.Quote(filepath.Join(dir, "counter"))
	fail := "  echo 'server exited unexpectedly' >&2; exit 1 ;;\n"
	if !startFails {
		fail = "  n=$(cat " + counter + " 2>/dev/null || echo 0); n=$((n + 1))\n" +
			"  echo \"$n\" > " + counter + "\n" +
			"  id=\"@$n\"; : > " + testenv.Quote(windows) + "/\"$id\"\n" +
			"  for a; do cmd=$a; done\n" +
			"  " + testenv.PgrunPrefix() + " sh -c \"$cmd\"'; rm -f \"$0\"' " + testenv.Quote(windows) + "/\"$id\" > /dev/null 2>&1 < /dev/null &\n" +
			"  echo \"$id\" ;;\n"
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + testenv.Quote(log) + "\n" +
		"if [ \"$1\" = -L ]; then shift 2; fi\n" +
		"case \"$1\" in\n" +
		"new-window)\n" + fail +
		"kill-window) rm -f " + testenv.Quote(windows) + "/\"$3\" ;;\n" +
		"list-windows) ls -1 " + testenv.Quote(windows) + "; exit 0 ;;\n" +
		"*) echo \"unexpected tmux call: $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "tmux")
	testenv.WriteExecutable(t, bin, []byte(script))
	gateTmuxDrainWindows(t, windows)
	return &host.Tmux{Bin: bin, Session: "work", Out: io.Discard, Poll: time.Millisecond,
		StartTimeout: 2 * time.Second, Grace: 200 * time.Millisecond, Pgrun: testenv.PgrunArgv()}, log
}

// gateTmuxDrainWindows waits, when the test ends, until every fake window has closed, so
// no background command still writes into the temporary directory being removed.
func gateTmuxDrainWindows(t *testing.T, windows string) {
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			entries, err := os.ReadDir(windows)
			if err != nil {
				t.Errorf("read fake tmux windows: %v", err)
				return
			}
			if len(entries) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("fake tmux windows still open after the test: %d", len(entries))
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

// gateTmuxWindowNames returns the names of the windows the fake tmux was asked to open, in
// order.
func gateTmuxWindowNames(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "new-window ") {
			continue
		}
		_, after, ok := strings.Cut(l, " -n ")
		if !ok {
			t.Fatalf("new-window without a name: %q", l)
		}
		name, _, _ := strings.Cut(after, " -c ")
		names = append(names, name)
	}
	return names
}

// gateTmuxExitCodes returns the exit codes the shared wrapper wrote into logDir, sorted.
func gateTmuxExitCodes(t *testing.T, logDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(logDir, "herdr-*.exit"))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, strings.TrimSpace(string(b)))
	}
	sort.Strings(codes)
	return codes
}

func TestTmuxGateRunsEachCommandInWindowNamedWithLabel(t *testing.T) {
	t.Parallel()
	tm, log := gateTmux(t, false)
	o := opts(t, "echo one", "echo two")
	o.Host, o.Label = tm, "g29/t a1 gate"
	r := Run(context.Background(), o)
	if !r.Passed || len(r.Commands) != 2 {
		t.Fatalf("result = %+v", r)
	}
	want := []string{"mill g29/t a1 gate 1", "mill g29/t a1 gate 2"}
	if got := gateTmuxWindowNames(t, log); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("window names = %q, want %q", got, want)
	}
	if r.Commands[1].Tail != "two" {
		t.Fatalf("second command's output = %q, want it run in its window", r.Commands[1].Tail)
	}
}

func TestTmuxGateResultComesFromExitFiles(t *testing.T) {
	t.Parallel()
	tm, _ := gateTmux(t, false)
	o := opts(t, "echo boom; exit 4", "exit 0")
	o.Host = tm
	r := Run(context.Background(), o)
	if r.Passed || r.Commands[0].Exit != 4 || r.Commands[0].Tail != "boom" || r.Commands[1].Exit != 0 {
		t.Fatalf("result = %+v, want the commands' exit codes 4 and 0", r)
	}
	if got := gateTmuxExitCodes(t, o.LogDir); fmt.Sprint(got) != "[0 4]" {
		t.Fatalf("exit files = %q, want [0 4] written by the window's wrapper", got)
	}
}

func TestTmuxGateStartFailureFallsBackToExecWithNote(t *testing.T) {
	t.Parallel()
	tm, log := gateTmux(t, true)
	o := opts(t, "echo from exec; exit 3")
	o.Host, o.Fallback, o.Label = tm, host.NewExec(), "g29/t a1 gate"
	r := Run(context.Background(), o)
	if got := gateTmuxWindowNames(t, log); len(got) != 2 {
		t.Fatalf("tmux was asked for %d window(s), want 2 (one retry)", len(got))
	}
	c := r.Commands[0]
	if c.StartFailed || c.Error != "" || c.Exit != 3 || c.Tail != "from exec" {
		t.Fatalf("command = %+v, want the real result from exec", c)
	}
	if !strings.Contains(c.Note, "tmux could not start it") || !strings.HasSuffix(c.Note, "ran on exec") {
		t.Fatalf("note = %q", c.Note)
	}
	if r.Passed {
		t.Fatal("gate passed with a failing command")
	}
}

func TestTmuxGateStartFailureFallbackPassesGate(t *testing.T) {
	t.Parallel()
	tm, _ := gateTmux(t, true)
	o := opts(t, "true")
	o.Host, o.Fallback = tm, host.NewExec()
	r := Run(context.Background(), o)
	if !r.Passed || r.Commands[0].Note == "" {
		t.Fatalf("result = %+v, want a pass with a note", r)
	}
}

func TestTmuxGateStartFailureOnEveryHostIsMarkedStartFailed(t *testing.T) {
	t.Parallel()
	tm, log := gateTmux(t, true)
	o := opts(t, "true")
	o.Host, o.Fallback = tm, host.NewExec()
	o.Dir = "/nonexistent/dir" // exec cannot start it either
	r := Run(context.Background(), o)
	c := r.Commands[0]
	if r.Passed || !c.StartFailed || !strings.Contains(c.Error, "could not run") || !strings.Contains(c.Error, "nor could exec") {
		t.Fatalf("command = %+v", c)
	}
	if got := gateTmuxWindowNames(t, log); len(got) != 2 {
		t.Fatalf("tmux was asked for %d window(s), want 2", len(got))
	}
}
