// Package project locates a repository and its .mill/ directory.
package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/msidorkevich/mill/internal/gitx"
)

// Dir is the name of mill's directory at the repository root.
const Dir = ".mill"

// ErrNotGitRepo is returned when the start directory is not inside a git work tree.
var ErrNotGitRepo = errors.New("not inside a git repository (run `git init` first)")

// ErrNotInitialized is returned when the repository has no .mill/config.toml.
var ErrNotInitialized = errors.New("mill is not initialized in this repository (run `mill init`)")

// Paths are the locations mill uses inside one repository.
type Paths struct {
	Root      string // repository root (git top level), symlinks resolved
	MillDir   string // <root>/.mill
	Config    string // <root>/.mill/config.toml
	DB        string // <root>/.mill/mill.db
	Runs      string // <root>/.mill/runs (ST3: logs, diffs, agent results)
	GitIgnore string // <root>/.mill/.gitignore
	RunLock   string // <root>/.mill/run.lock (held while a run is active)
}

// PathsAt returns the paths for a repository root.
func PathsAt(root string) Paths {
	m := filepath.Join(root, Dir)
	return Paths{
		Root:      root,
		MillDir:   m,
		Config:    filepath.Join(m, "config.toml"),
		DB:        filepath.Join(m, "mill.db"),
		Runs:      filepath.Join(m, "runs"),
		GitIgnore: filepath.Join(m, ".gitignore"),
		RunLock:   filepath.Join(m, "run.lock"),
	}
}

// GitRoot returns the git top level for dir, with symlinks resolved. It is GitRootContext
// without a context, so its answer is never cached.
func GitRoot(dir string) (string, error) {
	return GitRootContext(context.Background(), dir)
}

// GitRootContext returns the git top level for dir, with symlinks resolved, through
// gitx.TopLevel: the answer is reused when ctx carries a gitx cache. Outside a git work tree
// the error is ErrNotGitRepo.
func GitRootContext(ctx context.Context, dir string) (string, error) {
	root, err := gitx.TopLevel(ctx, dir)
	if err != nil {
		var nr *gitx.NotRepositoryError
		if errors.As(err, &nr) {
			return "", ErrNotGitRepo
		}
		return "", fmt.Errorf("find the repository of %s: %w", dir, err)
	}
	return root, nil
}

// Find returns the paths of the initialized repository containing dir. It is FindContext
// without a context.
func Find(dir string) (Paths, error) {
	return FindContext(context.Background(), dir)
}

// FindContext returns the paths of the initialized repository containing dir, looking the
// repository up through GitRootContext.
func FindContext(ctx context.Context, dir string) (Paths, error) {
	root, err := GitRootContext(ctx, dir)
	if err != nil {
		return Paths{}, err
	}
	p := PathsAt(root)
	if _, err := os.Stat(p.Config); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, ErrNotInitialized
		}
		return p, err
	}
	return p, nil
}
