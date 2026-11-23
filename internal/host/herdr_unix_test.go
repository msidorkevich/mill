//go:build unix

package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeHerdr writes a fake herdr binary into a temp dir. It logs its argv (one call per
// line) to the returned log file, prints split JSON, runs `sh <wrapper>` in the background
// for `pane run` (mirroring the pane to pane.out) and records `pane close`. split overrides
// the split behaviour with a shell snippet.
func fakeHerdr(t *testing.T, split string) (h *Herdr, log, dir string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "herdr.log")
	if split == "" {
		split = `echo '{"result":{"pane":"p1"}}'`
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + shQuote(log) + "\n" +
		"case \"$1 $2\" in\n" +
		"'pane split') " + split + " ;;\n" +
		"'pane run') \"$4\" \"$5\" > " + shQuote(filepath.Join(dir, "pane.out")) + " 2>&1 < /dev/null & ;;\n" +
		"'pane close') ;;\n" +
		"*) echo \"unexpected herdr call: $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Herdr{Bin: bin, Poll: 10 * time.Millisecond, StartTimeout: 10 * time.Second}, log, dir
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
	t.Setenv("MILL_TEST_OUTSIDE", "leak")
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
	if want := "pane split --current --direction right --no-focus --cwd " + dir; calls[0] != want {
		t.Fatalf("split call = %q, want %q", calls[0], want)
	}
	wrappers, _ := filepath.Glob(filepath.Join(logs, "herdr-*.sh"))
	if len(wrappers) != 1 {
		t.Fatalf("wrapper scripts in log dir = %v", wrappers)
	}
	if want := "pane run p1 sh " + wrappers[0]; calls[1] != want {
		t.Fatalf("run call = %q, want %q", calls[1], want)
	}
	if calls[2] != "pane close p1" {
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
	if got := read(t, filepath.Join(fake, "pane.out")); got != want {
		t.Fatalf("pane mirror = %q, want %q", got, want)
	}
	if w, _ := filepath.Glob(filepath.Join(dir, "wrap", "herdr-*.sh")); len(w) != 1 {
		t.Fatalf("wrapper not in LogDir: %v", w)
	}
}

func TestHerdrKillStopsProcessGroupAndClosesPane(t *testing.T) {
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
	if calls[len(calls)-1] != "pane close p1" {
		t.Fatalf("herdr calls = %q, want pane close p1 last", calls)
	}
}

func TestHerdrContextCancelKills(t *testing.T) {
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
	if calls := herdrLog(t, log); calls[len(calls)-1] != "pane close p1" {
		t.Fatalf("herdr calls = %q", calls)
	}
}

func TestHerdrStartErrors(t *testing.T) {
	for _, tc := range []struct {
		name, split, want string
	}{
		{"split fails", `echo 'no current pane' >&2; exit 1`, "no current pane"},
		{"no pane field", `echo '{"result":{}}'`, ".result.pane"},
		{"not json", `echo 'garbage'`, ".result.pane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh, _, _ := fakeHerdr(t, tc.split)
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
	hh, log, _ := fakeHerdr(t, "")
	// A herdr that accepts `pane run` but never runs the wrapper.
	script := "#!/bin/sh\necho \"$*\" >> " + shQuote(log) + "\n[ \"$2\" = split ] && echo '{\"result\":{\"pane\":\"p1\"}}'\nexit 0\n"
	if err := os.WriteFile(hh.Bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	hh.StartTimeout = 200 * time.Millisecond
	_, err := hh.Start(context.Background(), ProcSpec{Argv: []string{"/bin/true"}, StdoutPath: filepath.Join(t.TempDir(), "out")})
	if err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("err = %v, want a pid timeout", err)
	}
	if calls := herdrLog(t, log); calls[len(calls)-1] != "pane close p1" {
		t.Fatalf("pane not closed after failed start: %q", calls)
	}
}

func TestSelect(t *testing.T) {
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
