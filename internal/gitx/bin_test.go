package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestResolveGitBinary(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	file := func(name string, mode os.FileMode) string {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	target := file("real-git", 0o755)
	onPath := file("path-git", 0o755)
	plain := file("not-executable", 0o644)
	dir := filepath.Join(tmp, "directory")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("failed")

	tests := []struct {
		name      string
		goos      string
		path      string
		pathErr   error
		found     string
		findErr   error
		want      string
		wantCalls int
	}{
		{name: "darwin shim uses the xcrun path", goos: "darwin", path: "/usr/bin/git", found: target, want: target, wantCalls: 1},
		{name: "darwin uncleaned shim path", goos: "darwin", path: "/usr/bin/../bin//git", found: target, want: target, wantCalls: 1},
		{name: "darwin non-shim PATH git", goos: "darwin", path: onPath, found: target, want: onPath},
		{name: "xcrun fails", goos: "darwin", path: "/usr/bin/git", found: target, findErr: failed, want: "/usr/bin/git", wantCalls: 1},
		{name: "xcrun returns nothing", goos: "darwin", path: "/usr/bin/git", want: "/usr/bin/git", wantCalls: 1},
		{name: "xcrun returns a relative path", goos: "darwin", path: "/usr/bin/git", found: "git", want: "/usr/bin/git", wantCalls: 1},
		{name: "xcrun returns a missing path", goos: "darwin", path: "/usr/bin/git", found: filepath.Join(tmp, "missing"), want: "/usr/bin/git", wantCalls: 1},
		{name: "xcrun returns a non-executable file", goos: "darwin", path: "/usr/bin/git", found: plain, want: "/usr/bin/git", wantCalls: 1},
		{name: "xcrun returns a directory", goos: "darwin", path: "/usr/bin/git", found: dir, want: "/usr/bin/git", wantCalls: 1},
		{name: "non-darwin /usr/bin/git", goos: "linux", path: "/usr/bin/git", found: target, want: "/usr/bin/git"},
		{name: "LookPath fails", goos: "darwin", pathErr: failed, found: target, want: "git"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := resolver{
				lookPath: func(name string) (string, error) {
					if name != "git" {
						t.Errorf("lookPath(%q), want git", name)
					}
					return tc.path, tc.pathErr
				},
				xcrunFind: func() (string, error) {
					calls++
					return tc.found, tc.findErr
				},
				goos:   tc.goos,
				isExec: isExecutable,
			}.resolve()
			if got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("xcrun called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestBinIsTheSameForConcurrentCallers(t *testing.T) {
	t.Parallel()
	got := make([]string, 50)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = Bin()
		}()
	}
	wg.Wait()
	for i, g := range got {
		if g == "" || g != got[0] {
			t.Fatalf("Bin() call %d = %q, call 0 = %q; want one non-empty answer", i, g, got[0])
		}
	}
}
