package testenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestWriteExecutableRunsWhileOthersFork(t *testing.T) {
	const goroutines = 32
	rounds := 20
	if runtime.GOOS == "darwin" {
		// macOS scans every new executable on its first exec, which takes up to seconds per
		// file; fewer rounds keep the test short there.
		rounds = 4
	}
	dir := t.TempDir()
	errs := make(chan error, goroutines*rounds)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if g%2 == 1 {
					if err := exec.Command("true").Run(); err != nil {
						errs <- fmt.Errorf("fork true: %w", err)
					}
					continue
				}
				path := filepath.Join(dir, fmt.Sprintf("script-%d-%d", g, r))
				want := fmt.Sprintf("ran %d %d", g, r)
				WriteExecutable(t, path, []byte("#!/bin/sh\necho '"+want+"'\n"))
				var out strings.Builder
				cmd := exec.Command(path)
				cmd.Stdout = &out
				if err := StartRetry(cmd); err != nil {
					errs <- err
					continue
				}
				if err := cmd.Wait(); err != nil {
					errs <- fmt.Errorf("run %s: %w", path, err)
					continue
				}
				if got := strings.TrimSpace(out.String()); got != want {
					errs <- fmt.Errorf("run %s: output %q, want %q", path, got, want)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if errors.Is(err, syscall.ETXTBSY) {
			t.Errorf("ETXTBSY: %v", err)
		} else {
			t.Error(err)
		}
	}
}

func TestWriteExecutableLeavesOnlyTheFinalFileWithMode0755(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	WriteExecutable(t, path, []byte("#!/bin/sh\nexit 0\n"))
	WriteExecutable(t, path, []byte("#!/bin/sh\nexit 3\n"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode = %o, want 755", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "tool" {
		t.Fatalf("directory holds %v, want only tool", entries)
	}
	err = exec.Command(path).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("run = %v, want exit status 3 from the rewritten file", err)
	}
}

func TestStartRetryReturnsOtherErrorsAtOnceNamingThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	err := StartRetry(exec.Command(path))
	if err == nil {
		t.Fatal("StartRetry of a missing file succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name %s", err, path)
	}
	if strings.Contains(err.Error(), "busy") {
		t.Fatalf("error %q calls a missing file busy", err)
	}
}
