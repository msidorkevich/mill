package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// CommandRunner runs mill commands on behalf of the TUI.
type CommandRunner interface {
	// Capture runs a short read command and returns its output and exit code. err is set only
	// when the command could not be run or waited on; a non-zero exit is reported in code.
	Capture(ctx context.Context, args []string) (stdout, stderr string, code int, err error)
	// Start starts a long command whose stdout and stderr stream to out.
	Start(args []string, out io.Writer) (Proc, error)
}

// Proc is a command started by CommandRunner.Start.
type Proc interface {
	// Interrupt sends SIGINT.
	Interrupt() error
	// Wait waits for the command to exit and returns its exit code. err is set only when the
	// command could not be waited on; a non-zero exit is reported in the code.
	Wait() (int, error)
	// Args returns the mill arguments the command was started with.
	Args() []string
}

// ExecRunner runs the mill executable in the project directory.
type ExecRunner struct {
	Exe string // path of the mill executable
	Dir string // project root the commands run in
}

// NewExecRunner returns a runner for the running mill binary (os.Executable) in dir.
func NewExecRunner(dir string) (*ExecRunner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the mill executable: %w", err)
	}
	return &ExecRunner{Exe: exe, Dir: dir}, nil
}

// Capture implements CommandRunner.
func (r *ExecRunner) Capture(ctx context.Context, args []string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, r.Exe, args...)
	cmd.Dir = r.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code, err := exitCode(cmd.Run())
	return out.String(), errb.String(), code, err
}

// Start implements CommandRunner.
func (r *ExecRunner) Start(args []string, out io.Writer) (Proc, error) {
	cmd := exec.Command(r.Exe, args...)
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mill %v: %w", args, err)
	}
	return &execProc{cmd: cmd, args: append([]string(nil), args...)}, nil
}

type execProc struct {
	cmd  *exec.Cmd
	args []string
}

func (p *execProc) Interrupt() error { return p.cmd.Process.Signal(os.Interrupt) }
func (p *execProc) Wait() (int, error) {
	return exitCode(p.cmd.Wait())
}
func (p *execProc) Args() []string { return p.args }

// exitCode turns an exec error into an exit code; only failures to run stay errors.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil // -1 when a signal ended the process
	}
	return -1, err
}
