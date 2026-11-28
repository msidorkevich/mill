//go:build unix

package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/testenv"
)

// realTabCreate and realTabClose are `herdr tab create` and `herdr tab close` responses
// captured from real herdr.
const (
	realTabCreate = `{"id":"cli:tab:create","result":{"root_pane":{"agent_status":"unknown","cwd":"/private/tmp","focused":false,"foreground_cwd":"/private/tmp","pane_id":"w8:pD8","revision":0,"scroll":{"max_offset_from_bottom":0,"offset_from_bottom":0,"viewport_rows":60},"tab_id":"w8:t2","terminal_id":"term_660f1263366931c0","workspace_id":"w8"},"tab":{"agent_status":"unknown","focused":false,"label":"mill-probe","number":2,"pane_count":1,"tab_id":"w8:t2","workspace_id":"w8"},"type":"tab_created"}}`
	realTabClose  = `{"id":"cli:tab:close","result":{"type":"ok"}}`
)

// fakeHerdr writes a fake herdr binary into a temp dir. It logs its argv (one call per
// line) to the returned log file, prints the captured tab create JSON, runs `sh <wrapper>`
// in the background in its own process group (through the testenv pgrun helper) for
// `pane run` (mirroring the pane to
// pane.out and writing the wrapper's pid to wrapper.pid) and answers `tab close`. create
// overrides the tab create behaviour with a shell snippet.
func fakeHerdr(t *testing.T, create string) (h *Herdr, log, dir string) {
	t.Helper()
	return fakeHerdrShell(t, create, `"$4"`)
}

// fakeHerdrShell is fakeHerdr whose `pane run` runs the wrapper with shell (a shell word)
// instead of the shell mill names.
func fakeHerdrShell(t *testing.T, create, shell string) (h *Herdr, log, dir string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "herdr.log")
	if create == "" {
		create = `echo '` + realTabCreate + `'`
	}
	// herdr's own environment holds a variable the worker must not see.
	script := "#!/bin/sh\nexport MILL_TEST_OUTSIDE=leak\n" +
		"echo \"$*\" >> " + shQuote(log) + "\n" +
		"case \"$1 $2\" in\n" +
		"'tab create') " + create + " ;;\n" +
		"'pane run') " + testenv.PgrunPrefix() + " " + shell + " \"$5\" > " + shQuote(filepath.Join(dir, "pane.out")) + " 2>&1 < /dev/null &\n" +
		"  echo \"$!\" > " + shQuote(filepath.Join(dir, "wrapper.pid")) + " ;;\n" +
		"'tab close') echo '" + realTabClose + "' ;;\n" +
		"*) echo \"unexpected herdr call: $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "herdr")
	testenv.WriteExecutable(t, bin, []byte(script))
	return &Herdr{Bin: bin, Workspace: "w8", Poll: 10 * time.Millisecond, StartTimeout: 10 * time.Second, Grace: 500 * time.Millisecond, Pgrun: testenv.PgrunArgv()}, log, dir
}

func herdrLog(t *testing.T, log string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(read(t, log)), "\n")
}

func waitHerdr(t *testing.T, h Handle) Exit {
	t.Helper()
	type res struct {
		ex  Exit
		err error
	}
	c := make(chan res, 1)
	go func() { ex, err := h.Wait(); c <- res{ex, err} }()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		return r.ex
	case <-time.After(20 * time.Second):
		t.Fatal("Wait did not return")
	}
	return Exit{}
}

func TestHerdrCapturesOutputExitCodeAndExactEnv(t *testing.T) {
	t.Parallel()
	hh, log, _ := fakeHerdr(t, "")
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	out, errp := filepath.Join(logs, "out"), filepath.Join(logs, "err")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := hh.Start(context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", "echo hello; echo oops >&2; exit 3"},
		Dir:        dir,
		Env:        []string{"PATH=/usr/bin:/bin"},
		StdoutPath: out,
		StderrPath: errp,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.PID() <= 0 {
		t.Fatalf("PID = %d", h.PID())
	}
	ex := waitHerdr(t, h)
	if ex.Code != 3 || ex.Signaled {
		t.Fatalf("exit = %+v, want code 3", ex)
	}
	if got := read(t, out); got != "before\nhello\n" {
		t.Fatalf("stdout = %q, want appended", got)
	}
	if got := read(t, errp); got != "oops\n" {
		t.Fatalf("stderr = %q", got)
	}

	calls := herdrLog(t, log)
	if len(calls) != 3 {
		t.Fatalf("herdr calls = %q", calls)
	}
	if want := "tab create --workspace w8 --label mill worker --cwd " + dir; calls[0] != want {
		t.Fatalf("create call = %q, want %q", calls[0], want)
	}
	wrappers, _ := filepath.Glob(filepath.Join(logs, "herdr-*.sh"))
	if len(wrappers) != 1 {
		t.Fatalf("wrapper scripts in log dir = %v", wrappers)
	}
	if want := "pane run w8:pD8 sh " + wrappers[0]; calls[1] != want {
		t.Fatalf("run call = %q, want %q", calls[1], want)
	}
	if calls[2] != "tab close w8:t2" {
		t.Fatalf("close call = %q", calls[2])
	}

	// The environment is exactly spec.Env.
	envOut := filepath.Join(dir, "env")
	env := []string{"PATH=/usr/bin:/bin", "MILL_A=has space", "MILL_B=it's"}
	h, err = hh.Start(context.Background(), ProcSpec{Argv: []string{"/usr/bin/env"}, Env: env, StdoutPath: envOut})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 0 {
		t.Fatalf("env exit = %+v", ex)
	}
	got := strings.Split(strings.TrimSpace(read(t, envOut)), "\n")
	sort.Strings(got)
	want := append([]string{}, env...)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("env = %q, want exactly %q", got, want)
	}
}

func TestHerdrStdinDirAndPaneMirror(t *testing.T) {
	t.Parallel()
	hh, _, fake := fakeHerdr(t, "")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	if err := os.WriteFile(in, []byte("prompt text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sh", "-c", "pwd; cat"}, Dir: dir, StdinPath: in, StdoutPath: out,
		Env: []string{"PATH=/usr/bin:/bin"}, LogDir: filepath.Join(dir, "wrap"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	want := dir + "\nprompt text\n"
	if got := read(t, out); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := read(t, filepath.Join(fake, "pane.out")); got != "mill worker: worker. Closing this tab stops the task.\n"+want {
		t.Fatalf("pane mirror = %q, want %q", got, want)
	}
	if w, _ := filepath.Glob(filepath.Join(dir, "wrap", "herdr-*.sh")); len(w) != 1 {
		t.Fatalf("wrapper not in LogDir: %v", w)
	}
}

func TestHerdrKillStopsProcessGroupAndClosesPane(t *testing.T) {
	t.Parallel()
	hh, log, _ := fakeHerdr(t, "")
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; wait"},
		Env:        []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, pidFile)
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	ex := waitHerdr(t, h)
	if !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	waitGone(t, childPID)
	calls := herdrLog(t, log)
	if calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, want tab close w8:t2 last", calls)
	}
}

func TestHerdrContextCancelKills(t *testing.T) {
	t.Parallel()
	hh, log, _ := fakeHerdr(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	h, err := hh.Start(ctx, ProcSpec{Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if ex := waitHerdr(t, h); !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	if calls := herdrLog(t, log); calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q", calls)
	}
}

func TestHerdrStartErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, create, want string
	}{
		{"create fails", `echo 'no such workspace' >&2; exit 1`, "no such workspace"},
		{"no pane field", `echo '{"result":{"tab":{"tab_id":"w8:t2"}}}'`, ".result.root_pane.pane_id"},
		{"no tab field", `echo '{"result":{"root_pane":{"pane_id":"w8:pD8"}}}'`, ".result.tab.tab_id"},
		{"not json", `echo 'garbage'`, ".result.root_pane.pane_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh, _, _ := fakeHerdr(t, tc.create)
			_, err := hh.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, StdoutPath: filepath.Join(t.TempDir(), "out")})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	hh, _, _ := fakeHerdr(t, "")
	if _, err := hh.Start(context.Background(), ProcSpec{}); err == nil {
		t.Fatal("empty argv accepted")
	}
}

func TestHerdrStartFailsWhenPidNeverAppears(t *testing.T) {
	t.Parallel()
	hh, log, _ := fakeHerdr(t, "")
	// A herdr that accepts `pane run` but never runs the wrapper.
	script := "#!/bin/sh\necho \"$*\" >> " + shQuote(log) + "\n[ \"$2\" = create ] && echo '" + realTabCreate + "'\nexit 0\n"
	testenv.WriteExecutable(t, hh.Bin, []byte(script))
	hh.StartTimeout = 200 * time.Millisecond
	_, err := hh.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, StdoutPath: filepath.Join(t.TempDir(), "out")})
	if err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("err = %v, want a pid timeout", err)
	}
	if calls := herdrLog(t, log); calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("pane not closed after failed start: %q", calls)
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "HERDR_ENV" {
				return v
			}
			return ""
		}
	}
	found := func(string) (string, error) { return "/opt/bin/herdr", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, tc := range []struct {
		kind     string
		getenv   func(string) string
		lookPath func(string) (string, error)
		host     string
		desc     string
		err      string
	}{
		{"auto", env("1"), found, "herdr", "auto → herdr", ""},
		{"auto", env(""), found, "exec", "auto → exec", ""},
		{"auto", env("1"), missing, "exec", "auto → exec", ""},
		{"exec", env("1"), found, "exec", "exec", ""},
		{"exec", env(""), missing, "exec", "exec", ""},
		{"herdr", env("1"), found, "herdr", "herdr", ""},
		{"herdr", env(""), found, "", "", "host.kind = herdr needs mill to run inside a herdr pane"},
		{"herdr", env("1"), missing, "", "", `set host.kind = "auto" or "exec"`},
		{"bogus", env("1"), found, "", "", "unknown host.kind"},
	} {
		h, desc, err := Select(tc.kind, tc.getenv, tc.lookPath)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("Select(%q): err = %v, want %q", tc.kind, err, tc.err)
			}
			continue
		}
		if err != nil || h.Name() != tc.host || desc != tc.desc {
			t.Errorf("Select(%q) = %v %q %v, want %s %q", tc.kind, h, desc, err, tc.host, tc.desc)
		}
		if hd, ok := h.(*Herdr); ok && hd.Bin != "/opt/bin/herdr" {
			t.Errorf("herdr Bin = %q, want the PATH lookup", hd.Bin)
		}
	}
	if !InHerdr(env("1"), found) || InHerdr(env("0"), found) || InHerdr(env("1"), missing) {
		t.Error("InHerdr disagrees with HERDR_ENV and PATH")
	}
}

// waitHerdrResult waits for Wait and returns its result and how long it took.
func waitHerdrResult(t *testing.T, h Handle) (Exit, error, time.Duration) {
	t.Helper()
	type res struct {
		ex  Exit
		err error
	}
	start := time.Now()
	c := make(chan res, 1)
	go func() { ex, err := h.Wait(); c <- res{ex, err} }()
	select {
	case r := <-c:
		return r.ex, r.err, time.Since(start)
	case <-time.After(20 * time.Second):
		t.Fatal("Wait did not return")
	}
	return Exit{}, nil, 0
}

func TestHerdrRunsEachWorkerInOwnLabelledTabWithNotice(t *testing.T) {
	t.Parallel()
	hh, log, fake := fakeHerdr(t, "")
	dir := t.TempDir()
	h, err := hh.Start(context.Background(), ProcSpec{
		Name: "g12/docs a1 implementer", Argv: []string{"/bin/sh", "-c", "echo output"}, Dir: dir,
		Env: []string{"PATH=/usr/bin:/bin"}, StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	calls := herdrLog(t, log)
	if want := "tab create --workspace w8 --label mill g12/docs a1 implementer --cwd " + dir; calls[0] != want {
		t.Fatalf("create call = %q, want %q", calls[0], want)
	}
	if !strings.HasPrefix(calls[1], "pane run w8:pD8 sh ") || calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, want the run in root pane w8:pD8 and tab close w8:t2 last", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "--focus") || strings.Contains(c, "split") {
			t.Fatalf("herdr call %q focuses or splits the operator's tab", c)
		}
	}
	if got, want := read(t, filepath.Join(fake, "pane.out")), "mill worker: g12/docs a1 implementer. Closing this tab stops the task.\noutput\n"; got != want {
		t.Fatalf("pane shows %q, want %q", got, want)
	}
	if got := read(t, filepath.Join(dir, "out")); got != "output\n" {
		t.Fatalf("stdout = %q, want the command's output only", got)
	}

	// Without a workspace, herdr picks its own: no --workspace flag.
	hh.Workspace = ""
	h, err = hh.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, Dir: dir, StdoutPath: filepath.Join(dir, "out2")})
	if err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	calls = herdrLog(t, log)
	if want := "tab create --label mill worker --cwd " + dir; !slices.Contains(calls, want) {
		t.Fatalf("herdr calls = %q, want %q", calls, want)
	}
}

func TestHerdrWaitReportsClosedPane(t *testing.T) {
	t.Parallel()
	hh, log, fake := fakeHerdr(t, "")
	hh.Grace = 300 * time.Millisecond
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv:       []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + childFile + "; wait"},
		Env:        []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, childFile)
	// Closing the pane kills the wrapper before it can write its exit file.
	if err := KillGroup(waitForPID(t, filepath.Join(fake, "wrapper.pid"))); err != nil {
		t.Fatal(err)
	}
	ex, err, took := waitHerdrResult(t, h)
	if err == nil || !strings.Contains(err.Error(), "herdr pane running the worker") || !strings.Contains(err.Error(), "was closed or killed") {
		t.Fatalf("Wait err = %v, want the closed pane error", err)
	}
	if ex.Code != -1 || !ex.Signaled {
		t.Fatalf("exit = %+v, want code -1 signaled", ex)
	}
	if took > 5*time.Second {
		t.Fatalf("Wait took %s after the pane closed", took)
	}
	waitGone(t, childPID)
	if calls := herdrLog(t, log); calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, want tab close w8:t2 last", calls)
	}
}

func TestHerdrWaitReturnsAfterCancelWhenExitFileNeverAppears(t *testing.T) {
	t.Parallel()
	hh, log, fake := fakeHerdr(t, "")
	hh.StartTimeout, hh.Grace = 300*time.Millisecond, 300*time.Millisecond
	// A wrapper that reports a worker pid, stays alive and never writes its exit file; its
	// command line carries the wrapper path, as the host checks. It runs in its own process group (through the testenv pgrun helper),
	// killed at cleanup, and gives up on its own once
	// the fake's directory is gone or after a minute.
	wpid := filepath.Join(fake, "wrapper.pid")
	stuck := filepath.Join(fake, "stuck-wrapper.sh")
	body := "sleep 60 & echo \"$! $$\" > \"$1\"\n" + testenv.WaitForFileScript(fake, "never") + "\n"
	testenv.WriteExecutable(t, stuck, []byte(body))
	script := "#!/bin/sh\necho \"$*\" >> " + shQuote(log) + "\n" +
		"case \"$1 $2\" in\n" +
		"'tab create') echo '" + realTabCreate + "' ;;\n" +
		"'pane run') p=\"${5%.sh}.pid\"\n" +
		"  " + testenv.PgrunPrefix() + " sh " + shQuote(stuck) + " \"$p\" \"$5\" > /dev/null 2>&1 < /dev/null &\n" +
		"  echo \"$!\" > " + shQuote(wpid) + " ;;\n" +
		"'tab close') echo '" + realTabClose + "' ;;\n" +
		"esac\n"
	testenv.WriteExecutable(t, hh.Bin, []byte(script))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := hh.Start(ctx, ProcSpec{Argv: []string{"/bin/true"}, StdoutPath: filepath.Join(t.TempDir(), "out")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := KillGroup(waitForPID(t, wpid)); err != nil {
			t.Error(err)
		}
	})
	cancel()
	ex, err, took := waitHerdrResult(t, h)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Wait err = %v, want the cancelled error", err)
	}
	if ex.Code != -1 || !ex.Signaled {
		t.Fatalf("exit = %+v, want code -1 signaled", ex)
	}
	// StartTimeout plus grace, with slack for slow machines.
	if took > hh.StartTimeout+hh.Grace+2*time.Second {
		t.Fatalf("Wait took %s after cancel", took)
	}
	if calls := herdrLog(t, log); calls[len(calls)-1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, want tab close w8:t2 last", calls)
	}
}

func TestHerdrStartRecordsTabFileWithMillPidInLogDir(t *testing.T) {
	t.Parallel()
	hh, _, _ := fakeHerdr(t, "")
	millPid := strconv.Itoa(os.Getpid())
	dir := t.TempDir()
	logs := filepath.Join(dir, "a1")
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out"), LogDir: logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(logs, "herdr-*.tab"))
	if len(files) != 1 {
		t.Fatalf("tab files in log dir = %v, want one", files)
	}
	if got, want := strings.TrimSpace(read(t, files[0])), `{"tab":"w8:t2","workspace":"w8","mill_pid":`+millPid+`}`; got != want {
		t.Fatalf("tab file = %s, want %s", got, want)
	}
	tabs, err := RecordedHerdrTabs(logs)
	if err != nil || len(tabs) != 1 || tabs[0].Tab != "w8:t2" || tabs[0].Workspace != "w8" || tabs[0].File != files[0] || tabs[0].MillPID != os.Getpid() {
		t.Fatalf("RecordedHerdrTabs = %+v, %v", tabs, err)
	}
	if err := h.Kill(); err != nil {
		t.Fatal(err)
	}
	waitHerdr(t, h)
	// Once mill closed the tab, recovery has nothing left to close.
	if tabs, err := RecordedHerdrTabs(logs); err != nil || len(tabs) != 0 {
		t.Fatalf("after close: RecordedHerdrTabs = %+v, %v", tabs, err)
	}

	// Without a workspace the file records "".
	hh.Workspace = ""
	logs2 := filepath.Join(dir, "a2")
	h, err = hh.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out2"), LogDir: logs2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Kill(); err != nil {
			t.Error(err)
		}
		waitHerdr(t, h)
	}()
	files, _ = filepath.Glob(filepath.Join(logs2, "herdr-*.tab"))
	if len(files) != 1 || strings.TrimSpace(read(t, files[0])) != `{"tab":"w8:t2","workspace":"","mill_pid":`+millPid+`}` {
		t.Fatalf("tab files = %v", files)
	}
}

func TestHerdrStartFailsAndClosesTabWhenTabFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	logs := filepath.Join(t.TempDir(), "logs")
	// tab create puts a directory where the tab file goes.
	hh, log, _ := fakeHerdr(t, `for f in `+shQuote(logs)+`/herdr-*.sh; do mkdir "${f%.sh}.tab"; done; echo '`+realTabCreate+`'`)
	_, err := hh.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, LogDir: logs})
	if err == nil || !strings.Contains(err.Error(), "tab file "+logs) || !strings.Contains(err.Error(), ".tab") {
		t.Fatalf("err = %v, want it to name the tab file", err)
	}
	calls := herdrLog(t, log)
	if len(calls) != 2 || calls[1] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, want the tab closed and no pane run", calls)
	}
}

func TestRecordedHerdrTabsReadsEveryTabFileAndMillPid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if tabs, err := RecordedHerdrTabs(dir); err != nil || len(tabs) != 0 {
		t.Fatalf("empty dir: %+v, %v", tabs, err)
	}
	if tabs, err := RecordedHerdrTabs(filepath.Join(dir, "missing")); err != nil || len(tabs) != 0 {
		t.Fatalf("missing dir: %+v, %v", tabs, err)
	}
	for name, body := range map[string]string{
		"herdr-1.tab": `{"tab":"w8:t2","workspace":"w8"}`,
		"herdr-2.tab": `{"tab":"t5","workspace":"","mill_pid":4242}`,
		"herdr-1.pid": "1 2",
		"other.tab":   `{"tab":"nope"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tabs, err := RecordedHerdrTabs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 2 || tabs[0].Tab != "w8:t2" || tabs[0].Workspace != "w8" || tabs[0].MillPID != 0 ||
		tabs[1].Tab != "t5" || tabs[1].Workspace != "" || tabs[1].MillPID != 4242 {
		t.Fatalf("tabs = %+v", tabs)
	}
	if err := os.WriteFile(filepath.Join(dir, "herdr-3.tab"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordedHerdrTabs(dir); err == nil || !strings.Contains(err.Error(), "herdr-3.tab") {
		t.Fatalf("err = %v, want the bad file named", err)
	}
}

func TestCloseHerdrTabClosesOrReportsGone(t *testing.T) {
	t.Parallel()
	hh, log, dir := fakeHerdr(t, "")
	closed, err := CloseHerdrTab(HerdrTab{Tab: "w8:t2", Workspace: "w8", Bin: hh.Bin})
	if err != nil || !closed {
		t.Fatalf("close = %v, %v, want closed", closed, err)
	}
	if calls := herdrLog(t, log); len(calls) != 1 || calls[0] != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q", calls)
	}

	// herdr is not installed.
	closed, err = CloseHerdrTab(HerdrTab{Tab: "w8:t2", Bin: filepath.Join(dir, "no-herdr")})
	if err != nil || closed {
		t.Fatalf("no herdr: %v, %v, want not closed and no error", closed, err)
	}

	// herdr reports the tab does not exist.
	gone := filepath.Join(dir, "herdr-gone")
	testenv.WriteExecutable(t, gone, []byte("#!/bin/sh\necho \"error: tab $3 not found\" >&2\nexit 1\n"))
	closed, err = CloseHerdrTab(HerdrTab{Tab: "w8:t2", Bin: gone})
	if err != nil || closed {
		t.Fatalf("tab gone: %v, %v, want not closed and no error", closed, err)
	}

	// Any other failure is an error that says what to do.
	broken := filepath.Join(dir, "herdr-broken")
	testenv.WriteExecutable(t, broken, []byte("#!/bin/sh\necho 'server unreachable' >&2\nexit 1\n"))
	closed, err = CloseHerdrTab(HerdrTab{Tab: "w8:t2", Bin: broken})
	if err == nil || closed || !strings.Contains(err.Error(), "server unreachable") || !strings.Contains(err.Error(), "close herdr tab w8:t2 by hand") {
		t.Fatalf("broken herdr: %v, %v", closed, err)
	}
}

func TestNewHerdrDefaultStartTimeoutAllowsForLoad(t *testing.T) {
	t.Parallel()
	if got := NewHerdr().StartTimeout; got != 30*time.Second {
		t.Fatalf("NewHerdr().StartTimeout = %s, want 30s", got)
	}
	if got := (&Herdr{}).startTimeout(); got != 30*time.Second {
		t.Fatalf("unset StartTimeout waits %s, want 30s", got)
	}
}

func TestHerdrDefaultPgrunIsTheRunningBinaryAndWrapperHasNoJobControl(t *testing.T) {
	t.Parallel()
	if got := NewHerdr().Pgrun; len(got) != 0 {
		t.Fatalf("NewHerdr().Pgrun = %q, want empty", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := (&Herdr{}).pgrun()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{exe, "__pgrun", "--"}; !slices.Equal(prefix, want) {
		t.Fatalf("default pgrun prefix = %q, want %q", prefix, want)
	}
	files, err := writeWrapper(ProcSpec{Argv: []string{"/bin/true"}, LogDir: t.TempDir()}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	script := read(t, files.wrapper)
	if strings.Contains(script, "set -m") {
		t.Fatalf("wrapper uses job control:\n%s", script)
	}
	if want := shQuote(exe) + " '__pgrun' '--' sh " + shQuote(files.wrapper) + ` run "$$" &`; !strings.Contains(script, want) {
		t.Fatalf("wrapper does not start the inner copy with %q:\n%s", want, script)
	}
}

// dashHerdr is a fake herdr whose `pane run` runs the wrapper with dash, which prints job
// control errors in a pane without a tty. The test is skipped when dash is missing.
func dashHerdr(t *testing.T) (h *Herdr, log, dir string) {
	t.Helper()
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is not installed: the wrapper cannot be checked under dash")
	}
	return fakeHerdrShell(t, "", shQuote(dash))
}

// waitFileContains polls path until it contains want.
func waitFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("%s never contained %q; it holds %q", path, want, b)
}

func TestHerdrWrapperUnderDashLeadsItsOwnGroupAndKillStopsIt(t *testing.T) {
	t.Parallel()
	hh, _, fake := dashHerdr(t)
	dir := t.TempDir()
	workerFile, childFile := filepath.Join(dir, "worker.pid"), filepath.Join(dir, "child.pid")
	h, err := hh.Start(context.Background(), ProcSpec{
		Name: "dash worker",
		Argv: []string{"/bin/sh", "-c", "echo $$ > " + workerFile + "; echo worker output; sleep 30 & echo $! > " + childFile + "; wait"},
		Env:  []string{"PATH=/usr/bin:/bin"}, StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	hd := h.(*herdrHandle)
	workerPID, childPID := waitForPID(t, workerFile), waitForPID(t, childFile)

	f := strings.Fields(read(t, hd.files.pid))
	if len(f) != 2 {
		t.Fatalf("pid file holds %q, want the inner and the outer pid", f)
	}
	inner, err1 := strconv.Atoi(f[0])
	outer, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("pid file holds %q, want two pids", f)
	}
	pgid, err := syscall.Getpgid(workerPID)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != inner || h.PID() != inner {
		t.Fatalf("worker pgid = %d, pid file inner pid = %d, handle pid = %d: want all equal", pgid, inner, h.PID())
	}
	outerPgid, err := syscall.Getpgid(outer)
	if err != nil {
		t.Fatal(err)
	}
	if outerPgid == pgid {
		t.Fatalf("outer wrapper %d shares the worker's process group %d", outer, pgid)
	}

	pane := filepath.Join(fake, "pane.out")
	waitFileContains(t, pane, "worker output\n")
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if ex := waitHerdr(t, h); !ex.Signaled {
		t.Fatalf("exit = %+v, want signaled", ex)
	}
	waitGone(t, workerPID)
	waitGone(t, childPID)
	if _, err := os.Stat(hd.files.exit); err != nil {
		t.Fatalf("exit file after the group kill: %v", err)
	}
	got := read(t, pane)
	if want := "mill worker: dash worker. Closing this tab stops the task.\nworker output\n"; !strings.HasPrefix(got, want) {
		t.Fatalf("pane shows %q, want it to start with %q", got, want)
	}
	if l := strings.ToLower(got); strings.Contains(l, "job control") || strings.Contains(l, "tty") {
		t.Fatalf("pane shows job control noise: %q", got)
	}
}

func TestHerdrWrapperUnderDashRecordsExitWithoutJobControlNoise(t *testing.T) {
	t.Parallel()
	hh, _, fake := dashHerdr(t)
	dir := t.TempDir()
	h, err := hh.Start(context.Background(), ProcSpec{
		Argv: []string{"/bin/sh", "-c", "echo done; exit 4"},
		Env:  []string{"PATH=/usr/bin:/bin"}, StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 4 || ex.Signaled {
		t.Fatalf("exit = %+v, want code 4", ex)
	}
	if got, want := read(t, filepath.Join(fake, "pane.out")), "mill worker: worker. Closing this tab stops the task.\ndone\n"; got != want {
		t.Fatalf("pane shows %q, want %q", got, want)
	}
}

// fakeTerminal runs the wrapper as a plain child process; its window reports itself gone
// once closed is set.
type fakeTerminal struct {
	mu     sync.Mutex
	gone   bool
	closes int
}

func (*fakeTerminal) kind() string { return "fake" }

func (t *fakeTerminal) open(label, dir string, argv []string, record string) (window, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	return fakeWindow{t}, nil
}

type fakeWindow struct{ t *fakeTerminal }

func (w fakeWindow) close() error {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	w.t.closes++
	return nil
}

func (w fakeWindow) exists() (bool, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	return !w.t.gone, nil
}

func (fakeWindow) String() string { return "fake window w1" }

func TestWrapperProtocolRunsInAnyTerminalAndReportsClosedWindow(t *testing.T) {
	t.Parallel()
	cfg := wrapperConfig{poll: 10 * time.Millisecond, startTimeout: 10 * time.Second, grace: 300 * time.Millisecond, pgrun: testenv.PgrunArgv()}
	dir := t.TempDir()
	term := &fakeTerminal{}
	h, err := startWrapped(context.Background(), term, cfg, ProcSpec{
		Argv: []string{"/bin/sh", "-c", "echo hi; exit 5"}, Env: []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitHerdr(t, h); ex.Code != 5 || ex.Signaled {
		t.Fatalf("exit = %+v, want code 5", ex)
	}
	if got := read(t, filepath.Join(dir, "out")); got != "hi\n" {
		t.Fatalf("stdout = %q", got)
	}
	if term.closes != 1 {
		t.Fatalf("window closed %d times, want once", term.closes)
	}

	// A window that reports itself gone loses the attempt, even while its wrapper runs.
	term = &fakeTerminal{}
	h, err = startWrapped(context.Background(), term, cfg, ProcSpec{
		Argv: []string{"/bin/sleep", "30"}, Env: []string{"PATH=/usr/bin:/bin"},
		StdoutPath: filepath.Join(dir, "out2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	term.gone = true
	term.mu.Unlock()
	ex, err, _ := waitHerdrResult(t, h)
	if err == nil || !strings.Contains(err.Error(), "fake host:") || !strings.Contains(err.Error(), "fake window w1") || !strings.Contains(err.Error(), "was closed or killed") {
		t.Fatalf("Wait err = %v, want the closed window error", err)
	}
	if ex.Code != -1 || !ex.Signaled {
		t.Fatalf("exit = %+v, want code -1 signaled", ex)
	}
	waitGone(t, h.pid)
}
