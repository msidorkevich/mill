//go:build unix

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Exec runs workers as plain subprocesses, each in its own process group (HO2).
type Exec struct{}

// NewExec returns the exec host.
func NewExec() *Exec { return &Exec{} }

// Name implements Host.
func (*Exec) Name() string { return "exec" }

// Start implements Host.
func (*Exec) Start(ctx context.Context, spec ProcSpec) (Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("exec host: empty argv")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // nil would inherit mill's environment
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd.Process.Pid) }

	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	if spec.StdinPath != "" {
		f, err := os.Open(spec.StdinPath)
		if err != nil {
			return nil, fmt.Errorf("exec host: stdin: %w", err)
		}
		files = append(files, f)
		cmd.Stdin = f
	}
	for _, out := range []struct {
		path string
		dst  *io.Writer
	}{{spec.StdoutPath, &cmd.Stdout}, {spec.StderrPath, &cmd.Stderr}} {
		if out.path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out.path), 0o755); err != nil {
			closeAll()
			return nil, err
		}
		f, err := os.Create(out.path)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("exec host: %w", err)
		}
		files = append(files, f)
		*out.dst = f
	}

	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("exec host: start %s: %w", spec.Argv[0], err)
	}
	return &execHandle{cmd: cmd, close: closeAll}, nil
}

type execHandle struct {
	cmd   *exec.Cmd
	close func()
}

func (h *execHandle) PID() int { return h.cmd.Process.Pid }

func (h *execHandle) Wait() (Exit, error) {
	err := h.cmd.Wait()
	h.close()
	// Kill anything the process left running in its group (e.g. backgrounded children).
	_ = KillGroup(h.cmd.Process.Pid)
	if err == nil {
		return Exit{Code: 0}, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return Exit{Code: -1, Signaled: true}, nil
		}
		return Exit{Code: ee.ExitCode()}, nil
	}
	return Exit{Code: -1}, err
}

func (h *execHandle) Kill() error { return KillGroup(h.cmd.Process.Pid) }
