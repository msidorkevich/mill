//go:build unix

package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/testenv"
)

// fakeTmuxEnv returns a Getenv over env.
func fakeTmuxEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// fakeTmux writes a fake tmux binary into a temp dir. It logs its argv (one call per line)
// to the returned log file, drops a leading `-L <socket>`, and keeps the open windows (one
// id per line) in dir/windows and the sessions in dir/sessions. new-window prints the next
// id (@1, @2, ...) and runs the window's shell command in the background in its own
// process group (through the testenv pgrun helper), mirroring it to dir/window.out; the
// window goes from the list when its command ends, as in real tmux. kill-window removes a
// window and fails with tmux's message for an unknown one; list-windows prints the list;
// display-message prints the session "work"; has-session and new-session use the list.
func fakeTmux(t *testing.T) (tm *Tmux, log, dir string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "tmux.log")
	windows, sessions := shQuote(filepath.Join(dir, "windows")), shQuote(filepath.Join(dir, "sessions"))
	// remover drops window id $2 from the list in file $1.
	remover := filepath.Join(dir, "remove-window")
	testenv.WriteExecutable(t, remover, []byte("#!/bin/sh\nt=$(mktemp \"$1.XXXXXX\") || exit 1\ngrep -vx -- \"$2\" \"$1\" > \"$t\"\nmv \"$t\" \"$1\"\n"))
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + shQuote(log) + "\n" +
		"if [ \"$1\" = -L ]; then shift 2; fi\n" +
		"case \"$1\" in\n" +
		"new-window)\n" +
		"  n=$(cat " + shQuote(filepath.Join(dir, "counter")) + " 2>/dev/null || echo 0); n=$((n + 1))\n" +
		"  echo \"$n\" > " + shQuote(filepath.Join(dir, "counter")) + "\n" +
		"  id=\"@$n\"; echo \"$id\" >> " + windows + "\n" +
		"  for a; do cmd=$a; done\n" +
		"  " + testenv.PgrunPrefix() + " sh -c \"$cmd\"'; \"$0\" \"$1\" \"$2\"' " + shQuote(remover) + " " + windows + " \"$id\" > " + shQuote(filepath.Join(dir, "window.out")) + " 2>&1 < /dev/null &\n" +
		"  echo \"$id\" ;;\n" +
		"kill-window)\n" +
		"  if grep -qx -- \"$3\" " + windows + " 2>/dev/null; then " + shQuote(remover) + " " + windows + " \"$3\"\n" +
		"  else echo \"can't find window: $3\" >&2; exit 1; fi ;;\n" +
		"list-windows) cat " + windows + " 2>/dev/null; exit 0 ;;\n" +
		"display-message) echo work ;;\n" +
		"has-session) grep -qx -- \"${3#=}\" " + sessions + " 2>/dev/null || { echo \"can't find session: ${3#=}\" >&2; exit 1; } ;;\n" +
		"new-session) echo \"$4\" >> " + sessions + " ;;\n" +
		"*) echo \"unexpected tmux call: $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "tmux")
	testenv.WriteExecutable(t, bin, []byte(script))
	repo := filepath.Join(dir, "my.repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &Tmux{Bin: bin, RepoRoot: repo, Out: &out, Getenv: fakeTmuxEnv(nil),
		Poll: time.Millisecond, StartTimeout: 10 * time.Second, Grace: 200 * time.Millisecond, Pgrun: testenv.PgrunArgv()}, log, dir
}

// fakeTmuxLog returns the calls the fake tmux logged.
func fakeTmuxLog(t *testing.T, log string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(read(t, log)), "\n")
}

// fakeTmuxWindows returns the windows the fake tmux has open.
func fakeTmuxWindows(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "windows"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// fakeTmuxCalls returns the logged calls starting with prefix.
func fakeTmuxCalls(t *testing.T, log, prefix string) []string {
	t.Helper()
	var calls []string
	for _, c := range fakeTmuxLog(t, log) {
		if strings.HasPrefix(c, prefix) {
			calls = append(calls, c)
		}
	}
	return calls
}

func TestTmuxOpensDetachedNamedWindowAndReportsExitCode(t *testing.T) {
	t.Parallel()
	tm, log, fake := fakeTmux(t)
	tm.Session = "work"
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	out := filepath.Join(dir, "out")
	h, err := tm.Start(context.Background(), ProcSpec{
		Name: "g29/x a1 implementer", Argv: []string{"/bin/sh", "-c", "echo hello; exit 3"},
		Dir: dir, Env: []string{"PATH=/usr/bin:/bin"}, StdoutPath: out, LogDir: logs,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ex := waitHerdr(t, h)
	if ex.Code != 3 || ex.Signaled {
		t.Fatalf("exit = %+v, want code 3 from the exit file", ex)
	}
	if got := read(t, out); got != "hello\n" {
		t.Fatalf("stdout = %q", got)
	}
	wrappers, _ := filepath.Glob(filepath.Join(logs, "herdr-*.sh"))
	if len(wrappers) != 1 {
		t.Fatalf("wrapper scripts = %v", wrappers)
	}
	if exits, _ := filepath.Glob(filepath.Join(logs, "herdr-*.exit")); len(exits) != 1 || strings.TrimSpace(read(t, exits[0])) != "3" {
		t.Fatalf("exit files = %v, want the shared wrapper's exit file holding 3", exits)
	}
	calls := fakeTmuxCalls(t, log, "new-window")
	if want := "new-window -d -P -F #{window_id} -n mill g29/x a1 implementer -c " + dir + " -t =work: 'sh' '" + wrappers[0] + "'"; len(calls) != 1 || calls[0] != want {
		t.Fatalf("new-window calls = %q, want %q", calls, want)
	}
	if calls := fakeTmuxCalls(t, log, "kill-window"); len(calls) != 1 || calls[0] != "kill-window -t @1" {
		t.Fatalf("kill-window calls = %q", calls)
	}
	if !strings.HasPrefix(read(t, filepath.Join(fake, "window.out")), "mill worker: g29/x a1 implementer.") {
		t.Fatalf("window shows %q, want the worker notice", read(t, filepath.Join(fake, "window.out")))
	}
}

func TestTmuxSocketIsPassedOnEveryCall(t *testing.T) {
	t.Parallel()
	tm, log, _ := fakeTmux(t)
	tm.Socket = "mill-sock"
	h, err := tm.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	calls := fakeTmuxLog(t, log)
	for _, c := range calls {
		if !strings.HasPrefix(c, "-L mill-sock ") {
			t.Fatalf("call %q has no -L mill-sock; calls = %q", c, calls)
		}
	}
	if !strings.Contains(tm.Out.(*bytes.Buffer).String(), "tmux -L 'mill-sock' attach -t mill-my-repo") {
		t.Fatalf("hint = %q, want it to name the socket", tm.Out.(*bytes.Buffer).String())
	}
}

func TestTmuxKillStopsProcessGroupAndKillsWindow(t *testing.T) {
	t.Parallel()
	tm, log, fake := fakeTmux(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	h, err := tm.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; wait"},
		Env:  []string{"PATH=/usr/bin:/bin"}, StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, pidFile)
	if w := fakeTmuxWindows(t, fake); len(w) != 1 || w[0] != "@1" {
		t.Fatalf("open windows = %q, want @1", w)
	}
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if ex := waitHerdr(t, h); !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	waitGone(t, childPID)
	if calls := fakeTmuxCalls(t, log, "kill-window"); len(calls) != 1 || calls[0] != "kill-window -t @1" {
		t.Fatalf("kill-window calls = %q", calls)
	}
	if w := fakeTmuxWindows(t, fake); len(w) != 0 {
		t.Fatalf("open windows after Kill = %q", w)
	}
}

func TestTmuxInsideTmuxUsesTheCurrentSession(t *testing.T) {
	t.Parallel()
	tm, log, _ := fakeTmux(t)
	tm.Getenv = fakeTmuxEnv(map[string]string{"TMUX": "/tmp/tmux-1/default,1,0", "TMUX_PANE": "%5"})
	h, err := tm.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	calls := fakeTmuxLog(t, log)
	if calls[0] != "display-message -p -t %5 #{session_name}" {
		t.Fatalf("first call = %q, want the session of $TMUX_PANE", calls[0])
	}
	if !strings.Contains(calls[1], " -t =work: ") {
		t.Fatalf("new-window = %q, want it in session work", calls[1])
	}
	if c := fakeTmuxCalls(t, log, "new-session"); len(c) != 0 {
		t.Fatalf("new-session calls inside tmux = %q", c)
	}
	if out := tm.Out.(*bytes.Buffer).String(); out != "" {
		t.Fatalf("inside tmux printed %q, want no attach hint", out)
	}
}

func TestTmuxOutsideTmuxCreatesThenReusesRepoSessionAndHintsOnce(t *testing.T) {
	t.Parallel()
	tm, log, fake := fakeTmux(t)
	if got := TmuxSessionName("/x/my.repo:2"); got != "mill-my-repo-2" {
		t.Fatalf("TmuxSessionName = %q", got)
	}
	for i := 0; i < 2; i++ {
		h, err := tm.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		waitHerdr(t, h)
	}
	if c := fakeTmuxCalls(t, log, "has-session"); len(c) != 2 || c[0] != "has-session -t =mill-my-repo" {
		t.Fatalf("has-session calls = %q", c)
	}
	if c := fakeTmuxCalls(t, log, "new-session"); len(c) != 1 || c[0] != "new-session -d -s mill-my-repo -c "+tm.RepoRoot {
		t.Fatalf("new-session calls = %q, want one, the second Start reusing it", c)
	}
	for _, c := range fakeTmuxCalls(t, log, "new-window") {
		if !strings.Contains(c, " -t =mill-my-repo: ") {
			t.Fatalf("new-window = %q, want it in mill-my-repo", c)
		}
	}
	if got, want := tm.Out.(*bytes.Buffer).String(), "mill: workers run in tmux session mill-my-repo; watch them with: tmux attach -t mill-my-repo\n"; got != want {
		t.Fatalf("hint = %q, want %q once", got, want)
	}
	if read(t, filepath.Join(fake, "sessions")) != "mill-my-repo\n" {
		t.Fatalf("sessions = %q", read(t, filepath.Join(fake, "sessions")))
	}
}

func TestTmuxReusesExistingRepoSessionAndStillHints(t *testing.T) {
	t.Parallel()
	tm, log, fake := fakeTmux(t)
	if err := os.WriteFile(filepath.Join(fake, "sessions"), []byte("mill-my-repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := tm.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	if c := fakeTmuxCalls(t, log, "new-session"); len(c) != 0 {
		t.Fatalf("new-session calls = %q, want the existing session reused", c)
	}
	if !strings.Contains(tm.Out.(*bytes.Buffer).String(), "tmux attach -t mill-my-repo") {
		t.Fatalf("hint = %q", tm.Out.(*bytes.Buffer).String())
	}
}

func TestTmuxRecordsWindowFileAndRemovesItOnClose(t *testing.T) {
	t.Parallel()
	tm, _, _ := fakeTmux(t)
	tm.Socket = "sock"
	dir := t.TempDir()
	logs := filepath.Join(dir, "a1")
	h, err := tm.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out"), LogDir: logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(logs, "tmux-*.window"))
	if len(files) != 1 {
		t.Fatalf("window files = %v, want one", files)
	}
	want := `{"window":"@1","session":"mill-my-repo","socket":"sock","mill_pid":` + strconv.Itoa(os.Getpid()) + `}`
	if got := strings.TrimSpace(read(t, files[0])); got != want {
		t.Fatalf("window file = %s, want %s", got, want)
	}
	wrappers, _ := filepath.Glob(filepath.Join(logs, "herdr-*.sh"))
	if len(wrappers) != 1 || strings.TrimSuffix(filepath.Base(wrappers[0]), ".sh")[len("herdr-"):] != strings.TrimSuffix(filepath.Base(files[0]), ".window")[len("tmux-"):] {
		t.Fatalf("window file %v does not match wrapper %v", files, wrappers)
	}
	ws, err := RecordedTmuxWindows(logs)
	if err != nil || len(ws) != 1 || ws[0].Window != "@1" || ws[0].Session != "mill-my-repo" || ws[0].Socket != "sock" || ws[0].MillPID != os.Getpid() || ws[0].File != files[0] {
		t.Fatalf("RecordedTmuxWindows = %+v, %v", ws, err)
	}
	if err := h.Kill(); err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	if ws, err := RecordedTmuxWindows(logs); err != nil || len(ws) != 0 {
		t.Fatalf("after close: RecordedTmuxWindows = %+v, %v", ws, err)
	}
}

func TestRecordedTmuxWindowsReadsEveryWindowFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if ws, err := RecordedTmuxWindows(filepath.Join(dir, "missing")); err != nil || len(ws) != 0 {
		t.Fatalf("missing dir: %+v, %v", ws, err)
	}
	for name, body := range map[string]string{
		"tmux-1.window": `{"window":"@3","session":"work","socket":""}`,
		"tmux-2.window": `{"window":"@7","session":"mill-x","socket":"s","mill_pid":4242}`,
		"herdr-1.tab":   `{"tab":"t1"}`,
		"other.window":  `{"window":"@9"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := RecordedTmuxWindows(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Window != "@3" || ws[0].Session != "work" || ws[0].MillPID != 0 ||
		ws[1].Window != "@7" || ws[1].Socket != "s" || ws[1].MillPID != 4242 || ws[1].File != filepath.Join(dir, "tmux-2.window") {
		t.Fatalf("windows = %+v", ws)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmux-3.window"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordedTmuxWindows(dir); err == nil || !strings.Contains(err.Error(), "tmux-3.window") || !strings.Contains(err.Error(), "delete the file") {
		t.Fatalf("err = %v, want the bad file named with what to do", err)
	}
}

func TestCloseTmuxWindowClosesOrReportsGone(t *testing.T) {
	t.Parallel()
	tm, log, fake := fakeTmux(t)
	if err := os.WriteFile(filepath.Join(fake, "windows"), []byte("@4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	closed, err := CloseTmuxWindow(TmuxWindow{Window: "@4", Socket: "s", Bin: tm.Bin})
	if err != nil || !closed {
		t.Fatalf("close = %v, %v, want closed", closed, err)
	}
	if calls := fakeTmuxLog(t, log); len(calls) != 1 || calls[0] != "-L s kill-window -t @4" {
		t.Fatalf("tmux calls = %q", calls)
	}
	// The window is already gone.
	closed, err = CloseTmuxWindow(TmuxWindow{Window: "@4", Socket: "s", Bin: tm.Bin})
	if err != nil || closed {
		t.Fatalf("window gone: %v, %v, want not closed and no error", closed, err)
	}
	// tmux is not installed.
	closed, err = CloseTmuxWindow(TmuxWindow{Window: "@4", Bin: filepath.Join(fake, "no-tmux")})
	if err != nil || closed {
		t.Fatalf("no tmux: %v, %v, want not closed and no error", closed, err)
	}
	// Any other failure is an error that says what to do.
	broken := filepath.Join(fake, "tmux-broken")
	testenv.WriteExecutable(t, broken, []byte("#!/bin/sh\necho 'permission denied' >&2\nexit 1\n"))
	closed, err = CloseTmuxWindow(TmuxWindow{Window: "@4", Bin: broken})
	if err == nil || closed || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "close tmux window @4 by hand") {
		t.Fatalf("broken tmux: %v, %v", closed, err)
	}
}

func TestTmuxWaitReportsClosedWindow(t *testing.T) {
	t.Parallel()
	tm, _, fake := fakeTmux(t)
	h, err := tm.Start(context.Background(), ProcSpec{Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The operator closes the window: tmux no longer lists it.
	if err := os.WriteFile(filepath.Join(fake, "windows"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ex, err, _ := waitHerdrResult(t, h)
	if err == nil || !strings.Contains(err.Error(), "tmux window @1") || !ex.Signaled {
		t.Fatalf("Wait = %+v, %v, want a lost attempt naming the window", ex, err)
	}
}

func TestTmuxNameAndDefaults(t *testing.T) {
	t.Parallel()
	tm := NewTmux()
	if tm.Name() != "tmux" || tm.Bin != "tmux" || tm.Socket != "" || tm.Session != "" || tm.Getenv == nil || tm.Out != os.Stderr {
		t.Fatalf("NewTmux = %+v", tm)
	}
}

func TestTmuxRunsWorkerInRealTmuxOnPrivateSocket(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not on PATH; install tmux to run the real-tmux test")
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	socket := "mill-test-" + hex.EncodeToString(b)
	t.Cleanup(func() {
		out, err := exec.Command(bin, "-L", socket, "kill-server").CombinedOutput()
		if err != nil && !tmuxMissing(string(out)) {
			t.Errorf("kill tmux server %s: %v: %s", socket, err, out)
		}
	})
	repo := t.TempDir()
	var hint bytes.Buffer
	tm := &Tmux{Bin: bin, Socket: socket, RepoRoot: repo, Out: &hint, Getenv: fakeTmuxEnv(nil),
		Poll: 10 * time.Millisecond, StartTimeout: 10 * time.Second, Grace: 500 * time.Millisecond, Pgrun: testenv.PgrunArgv()}
	listWindows := func() string {
		out, err := exec.Command(bin, "-L", socket, "list-windows", "-a", "-F", "#{window_id} #{window_name}").CombinedOutput()
		if err != nil {
			t.Fatalf("list-windows: %v: %s", err, out)
		}
		return string(out)
	}

	h, err := tm.Start(context.Background(), ProcSpec{Name: "t a1 implementer", Argv: []string{"/bin/sh", "-c", "exit 3"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 3 {
		t.Fatalf("exit = %+v, want 3", ex)
	}

	logs := t.TempDir()
	h, err = tm.Start(context.Background(), ProcSpec{Name: "t a1 gate 1", Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"}, LogDir: logs})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := RecordedTmuxWindows(logs)
	if err != nil || len(ws) != 1 {
		t.Fatalf("RecordedTmuxWindows = %+v, %v", ws, err)
	}
	if got := listWindows(); !strings.Contains(got, ws[0].Window+" mill t a1 gate 1\n") {
		t.Fatalf("windows = %q, want %s named `mill t a1 gate 1`", got, ws[0].Window)
	}
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if ex := waitHerdr(t, h); !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	if got := listWindows(); strings.Contains(got, ws[0].Window+" ") {
		t.Fatalf("windows after Kill = %q, want %s gone", got, ws[0].Window)
	}
	if !strings.Contains(hint.String(), "attach -t "+TmuxSessionName(repo)) {
		t.Fatalf("hint = %q", hint.String())
	}
}
