package project

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestFindFromSubdirectory(t *testing.T) {
	root := gitInit(t)
	p := PathsAt(root)
	if err := os.MkdirAll(p.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("Find = %+v, want %+v", got, p)
	}
}

func TestFindNotInitialized(t *testing.T) {
	root := gitInit(t)
	p, err := Find(root)
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
	if p.Root != root {
		t.Fatalf("Root = %q, want %q", p.Root, root)
	}
}

func TestFindNotGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	// Make sure no parent of the temp dir is a repository for this test's git calls.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if _, err := Find(dir); !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("err = %v, want ErrNotGitRepo", err)
	}
}
