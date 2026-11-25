package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
)

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command(gitx.Bin(), "init", "-q", dir)
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
	t.Parallel()
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
	t.Parallel()
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
	// Serial: it sets GIT_CEILING_DIRECTORIES with t.Setenv. gitx drops GIT_* variables from
	// the environment of the git it runs, so this only holds for git run directly.
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

func TestGitRootOutsideRepositoryIsErrNotGitRepoAndNotCached(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GitRoot(dir); !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("GitRoot: err = %v, want ErrNotGitRepo", err)
	}
	ctx := gitx.WithCache(context.Background(), gitx.NewCache())
	if _, err := GitRootContext(ctx, dir); !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("GitRootContext: err = %v, want ErrNotGitRepo", err)
	}
	// The failure is not remembered: once dir is a repository, the same cache finds it.
	if out, err := exec.Command(gitx.Bin(), "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if root, err := GitRootContext(ctx, dir); err != nil || root != dir {
		t.Fatalf("GitRootContext after init = %q, %v; want %q", root, err, dir)
	}
}

func TestGitRootContextReusesCachedAnswer(t *testing.T) {
	t.Parallel()
	root := gitInit(t)
	var n atomic.Int64
	ctx := gitx.WithCounter(gitx.WithCache(context.Background(), gitx.NewCache()), &n)
	for i := 0; i < 3; i++ {
		got, err := GitRootContext(ctx, root)
		if err != nil || got != root {
			t.Fatalf("GitRootContext = %q, %v; want %q", got, err, root)
		}
	}
	if n.Load() != 1 {
		t.Errorf("git started %d times for three lookups, want 1", n.Load())
	}
}
