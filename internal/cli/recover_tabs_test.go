package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := killLeftoverWorkers(context.Background(), &out, s, herdrTabs{runs: runs, lookPath: lookPath}); err != nil {
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
	if err := killLeftoverWorkers(context.Background(), &out, s, herdrTabs{runs: runs, lookPath: noHerdr}); err != nil {
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
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	lookPath := func(string) (string, error) { return bin, nil }
	var out bytes.Buffer
	if err := killLeftoverWorkers(context.Background(), &out, s, herdrTabs{runs: runs, lookPath: lookPath}); err != nil {
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
	if err := killLeftoverWorkers(context.Background(), &out, s, herdrTabs{runs: filepath.Join(t.TempDir(), "runs"), lookPath: lookPath}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("herdr was called with no recorded tabs: %v", err)
	}
}
