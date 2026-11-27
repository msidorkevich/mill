//go:build unix

// Package pgrun starts a command as the leader of a new process group, so the caller can
// later stop the command and everything it started with one kill of the negative pgid.
package pgrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ErrNotFound marks an error from Exec for a command that cannot be found or run as a file.
var ErrNotFound = errors.New("command not found")

// Exec takes "[--] cmd args...", makes the current process the leader of a new process
// group and replaces it with cmd. It returns only on failure and prints nothing.
func Exec(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("no command given: pass the command to run after --, e.g. __pgrun -- cmd args")
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("%w: %q: %v; check the command name and PATH", ErrNotFound, args[0], err)
	}
	if err := syscall.Setpgid(0, 0); err != nil {
		return fmt.Errorf("setpgid(0, 0) before running %q: %v; run it from a process that may create a process group", args[0], err)
	}
	if err := syscall.Exec(path, args, os.Environ()); err != nil {
		return fmt.Errorf("exec %q (%s): %v; check that the file is executable", args[0], path, err)
	}
	return nil
}
