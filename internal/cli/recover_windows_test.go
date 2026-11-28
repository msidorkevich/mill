package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// recoverTmuxScript logs every call and fails kill-window of @gone the way tmux reports a
// window that no longer exists.
const recoverTmuxScript = `#!/bin/sh
echo "$*" >> "$(dirname "$0")/tmux.log"
for a in "$@"; do
	if [ "$a" = "@gone" ]; then
		echo "can't find window: @gone" >&2
		exit 1
	fi
done
exit 0
`

// recoverTmuxFake writes the fake tmux under the name bin and returns a LookPath that finds
// only it (never a real tmux or herdr), plus its call log.
func recoverTmuxFake(t *testing.T, bin string) (lookPath func(string) (string, error), log string) {
	t.Helper()
	dir := t.TempDir()
	testenv.WriteExecutable(t, filepath.Join(dir, bin), []byte(recoverTmuxScript))
	lookPath = func(file string) (string, error) {
		if file == bin {
			return filepath.Join(dir, bin), nil
		}
		return "", exec.ErrNotFound
	}
	return lookPath, filepath.Join(dir, "tmux.log")
}

// recoverTmuxWindow writes a tmux window file named tmux-<name>.window into runs/<rel>.
func recoverTmuxWindow(t *testing.T, runs, rel, name, body string) string {
	t.Helper()
	dir := filepath.Join(runs, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "tmux-"+name+".window")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// recoverTmuxCalls returns the logged tmux calls, none when tmux was never run.
func recoverTmuxCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func recoverTmuxBody(window string, millPID int) string {
	return `{"window":"` + window + `","session":"mill","socket":"","mill_pid":` + strconv.Itoa(millPID) + "}\n"
}

func TestRecoverClosesTmuxWindowOfDeadMill(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := recoverTmuxWindow(t, runs, "keeper", "1", recoverTmuxBody("@3", deadPid(t)))
	lookPath, log := recoverTmuxFake(t, "tmux")
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "closed tmux window @3 of keeper (mill ") || strings.Count(got, "\n") != 1 {
		t.Fatalf("output = %q, want one line closing tmux window @3 of keeper", got)
	}
	if calls := recoverTmuxCalls(t, log); len(calls) != 1 || calls[0] != "kill-window -t @3" {
		t.Fatalf("tmux calls = %q, want [kill-window -t @3]", calls)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("window file still there after close: %v", err)
	}
}

func TestRecoverClosesTmuxWindowOfRunningAttemptWithoutMillPID(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := recoverTmuxWindow(t, runs, filepath.Join("t1", "a2"), "1", `{"window":"@4","session":"mill","socket":"mill-x"}`)
	lookPath, log := recoverTmuxFake(t, "tmux")
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "closed tmux window @4 of t1 attempt 2 implementer\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if calls := recoverTmuxCalls(t, log); len(calls) != 1 || calls[0] != "-L mill-x kill-window -t @4" {
		t.Fatalf("tmux calls = %q, want [-L mill-x kill-window -t @4]", calls)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("window file still there after close: %v", err)
	}
}

func TestRecoverKeepsTmuxWindowOfLiveMill(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	// Both the running attempt's dir and another run dir: neither rule closes a live mill's window.
	files := []string{
		recoverTmuxWindow(t, runs, filepath.Join("t1", "a2"), "1", recoverTmuxBody("@5", os.Getpid())),
		recoverTmuxWindow(t, runs, "sync", "1", recoverTmuxBody("@6", os.Getpid())),
	}
	lookPath, log := recoverTmuxFake(t, "tmux")
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if calls := recoverTmuxCalls(t, log); len(calls) != 0 {
		t.Fatalf("tmux calls = %q, want none for a live mill", calls)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("window file of a live mill removed: %v", err)
		}
	}
}

func TestRecoverForgetsGoneTmuxWindowSilently(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := recoverTmuxWindow(t, runs, "keeper", "1", recoverTmuxBody("@gone", deadPid(t)))
	lookPath, log := recoverTmuxFake(t, "tmux")
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if calls := recoverTmuxCalls(t, log); len(calls) != 1 || calls[0] != "kill-window -t @gone" {
		t.Fatalf("tmux calls = %q, want [kill-window -t @gone]", calls)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record of a gone window kept: %v", err)
	}
}

func TestRecoverWithTmuxMissingKeepsRecord(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := recoverTmuxWindow(t, runs, "keeper", "1", recoverTmuxBody("@3", deadPid(t)))
	noTmux := func(string) (string, error) { return "", exec.ErrNotFound }
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: noTmux}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("window file removed without tmux: %v", err)
	}
}

func TestRecoverUsesConfiguredTmuxBinary(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := recoverTmuxWindow(t, runs, "keeper", "1", recoverTmuxBody("@3", deadPid(t)))
	lookPath, log := recoverTmuxFake(t, "my-tmux")
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath, tmuxBin: "my-tmux"}); err != nil {
		t.Fatal(err)
	}
	if calls := recoverTmuxCalls(t, log); len(calls) != 1 || calls[0] != "kill-window -t @3" {
		t.Fatalf("my-tmux calls = %q, want [kill-window -t @3]", calls)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("window file still there after close: %v", err)
	}
}

func TestRecoverClosesHerdrTabsAndTmuxWindowsTogether(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs, tab := recordTab(t, `{"tab":"w8:t2","workspace":"w8"}`)
	window := recoverTmuxWindow(t, runs, filepath.Join("t1", "a2"), "1", `{"window":"@7","session":"mill","socket":""}`)
	herdrPath, herdrLog := fakeHerdr(t)
	tmuxPath, tmuxLog := recoverTmuxFake(t, "tmux")
	lookPath := func(file string) (string, error) {
		if file == "herdr" {
			return herdrPath(file)
		}
		return tmuxPath(file)
	}
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	want := "closed herdr tab w8:t2 of t1 attempt 2 implementer\nclosed tmux window @7 of t1 attempt 2 implementer\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(herdrLog); err != nil || strings.TrimSpace(string(b)) != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, %v", b, err)
	}
	if calls := recoverTmuxCalls(t, tmuxLog); len(calls) != 1 || calls[0] != "kill-window -t @7" {
		t.Fatalf("tmux calls = %q", calls)
	}
	for _, f := range []string{tab, window} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("record %s still there after close: %v", f, err)
		}
	}
}
