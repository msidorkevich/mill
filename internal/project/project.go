// Package project locates a repository and its .mill/ directory.
package project

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// GitRoot returns the git top level for dir, with symlinks resolved.
func GitRoot(dir string) (string, error) {
	cmd := exec.Command(gitx.Bin(), "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(stderr.String(), "not a git repository") {
			return "", ErrNotGitRepo
		}
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	root := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, nil
}

// Find returns the paths of the initialized repository containing dir.
func Find(dir string) (Paths, error) {
	root, err := GitRoot(dir)
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
