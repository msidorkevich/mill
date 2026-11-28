package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/testenv"
)

// recordTab writes a herdr tab file into the run dir of t1 attempt 2, the attempt
// recoverStore leaves running, and returns the runs dir and the file.
func recordTab(t *testing.T, body string) (runs, file string) {
	t.Helper()
	runs = filepath.Join(t.TempDir(), "runs")
	dir := filepath.Join(runs, "t1", "a2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "herdr-123.tab")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return runs, file
}

func TestRecoverClosesRecordedHerdrTab(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs, file := recordTab(t, `{"tab":"w8:t2","workspace":"w8"}`)
	lookPath, log := fakeHerdr(t)
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "closed herdr tab w8:t2 of t1 attempt 2 implementer\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	b, err := os.ReadFile(log)
	if err != nil || strings.TrimSpace(string(b)) != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, %v, want tab close w8:t2", b, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tab file still there after close: %v", err)
	}
}

func TestRecoverWithHerdrUnavailablePrintsNothing(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs, file := recordTab(t, `{"tab":"w8:t2","workspace":"w8"}`)
	noHerdr := func(string) (string, error) { return "", errors.New("not found") }
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: noHerdr}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("tab file removed without herdr: %v", err)
	}
}

func TestRecoverWithHerdrTabAlreadyGonePrintsNothing(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs, _ := recordTab(t, `{"tab":"w8:t2","workspace":"w8"}`)
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\necho \"$*\" >> " + filepath.Join(dir, "herdr.log") + "\necho \"error: tab $3 not found\" >&2\nexit 1\n"
	testenv.WriteExecutable(t, bin, []byte(script))
	lookPath := func(string) (string, error) { return bin, nil }
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "herdr.log")); err != nil || strings.TrimSpace(string(b)) != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, %v", b, err)
	}
}

func TestRecoverWithoutRecordedHerdrTabsCallsNoHerdr(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	lookPath, log := fakeHerdr(t)
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: filepath.Join(t.TempDir(), "runs"), lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("herdr was called with no recorded tabs: %v", err)
	}
}

// deadPid returns the pid of a process that has exited and been reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if _, _, alive, err := host.ProcessInfo(pid); err != nil || alive {
		t.Fatalf("pid %d of an exited process: alive=%v, %v", pid, alive, err)
	}
	return pid
}

// writeTab writes a herdr tab file named herdr-<name>.tab into runs/<rel>.
func writeTab(t *testing.T, runs, rel, name, body string) string {
	t.Helper()
	dir := filepath.Join(runs, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "herdr-"+name+".tab")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestRecoverTabsClosesTabsOfDeadMillInEveryRunDir(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	dead := strconv.Itoa(deadPid(t))
	// None of these dirs is a running attempt's: only the dead mill pid gets them closed.
	closedFiles := map[string]string{
		"g1/a/a1":      "w8:t11", // pipeline attempt
		"g1/a/keeper":  "w8:t12", // keeper gate
		"g1/a/sync":    "w8:t13", // sync gate
		"combined/wt1": "w8:t14", // combined check
	}
	for rel, tab := range closedFiles {
		closedFiles[rel] = writeTab(t, runs, rel, "1", `{"tab":"`+tab+`","workspace":"w8","mill_pid":`+dead+`}`)
	}
	lookPath, log := fakeHerdr(t)
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	slices.Sort(calls)
	if want := []string{"tab close w8:t11", "tab close w8:t12", "tab close w8:t13", "tab close w8:t14"}; !slices.Equal(calls, want) {
		t.Fatalf("herdr calls = %q, want %q", calls, want)
	}
	for rel, file := range closedFiles {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("tab file in %s still there after close: %v", rel, err)
		}
		if !strings.Contains(out.String(), "of "+rel+" (mill "+dead+" is gone)\n") {
			t.Errorf("output does not name %s:\n%s", rel, out.String())
		}
	}
	if n := strings.Count(out.String(), "closed herdr tab "); n != 4 {
		t.Fatalf("output has %d closed lines, want 4:\n%s", n, out.String())
	}
}

func TestRecoverTabsLeavesTabsOfLiveMill(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	self := strconv.Itoa(os.Getpid())
	files := []string{
		// t1 attempt 2 is running, but its mill (this test) is alive.
		writeTab(t, runs, "t1/a2", "1", `{"tab":"w8:t21","workspace":"w8","mill_pid":`+self+`}`),
		writeTab(t, runs, "g1/a/sync", "1", `{"tab":"w8:t22","workspace":"w8","mill_pid":`+self+`}`),
		writeTab(t, runs, "combined/wt1", "1", `{"tab":"w8:t23","workspace":"w8","mill_pid":`+self+`}`),
		// No mill pid and not a running attempt's dir: today's behaviour leaves it.
		writeTab(t, runs, "g1/a/keeper", "1", `{"tab":"w8:t24","workspace":"w8"}`),
	}
	lookPath, log := fakeHerdr(t)
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none", out.String())
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		b, _ := os.ReadFile(log)
		t.Fatalf("herdr was called for tabs of a live mill: %q", b)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("tab file removed: %v", err)
		}
	}
}

func TestRecoverTabsClosesRunningAttemptTabOfDeadMill(t *testing.T) {
	t.Parallel()
	s, _ := recoverStore(t)
	runs := filepath.Join(t.TempDir(), "runs")
	file := writeTab(t, runs, "t1/a2", "1", `{"tab":"w8:t2","workspace":"w8","mill_pid":`+strconv.Itoa(deadPid(t))+`}`)
	lookPath, log := fakeHerdr(t)
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, hostWindows{runs: runs, lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "closed herdr tab w8:t2 of t1 attempt 2 implementer\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(log); err != nil || strings.TrimSpace(string(b)) != "tab close w8:t2" {
		t.Fatalf("herdr calls = %q, %v, want one tab close w8:t2", b, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tab file still there after close: %v", err)
	}
}
