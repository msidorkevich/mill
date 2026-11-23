package tui

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/store"
)

// execHerdr runs the herdr executable and captures its output.
func execHerdr(ctx context.Context, args []string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "herdr", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code, err := exitCode(cmd.Run())
	return out.String(), errb.String(), code, err
}

// openWorktree opens the task's worktree: in a new herdr pane when mill runs inside herdr,
// otherwise in a shell that suspends the TUI until it exits.
func (m *Model) openWorktree(taskID string) tea.Cmd {
	if taskID == "" {
		m.cmds.note("open worktree: select a task first")
		return nil
	}
	ctx := context.Background()
	b, err := m.opts.Store.GetBranch(ctx, taskID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		m.cmds.note("open worktree: %s has no worktree; nothing opened", taskID)
		return nil
	case err != nil:
		m.cmds.note("open worktree: read the branch of %s: %v", taskID, err)
		return nil
	case b.Worktree == "":
		m.cmds.note("open worktree: %s has no worktree (cleaned?); nothing opened", taskID)
		return nil
	}
	if host.InHerdr(m.opts.Getenv, m.opts.LookPath) {
		args := []string{"pane", "split", "--current", "--direction", "right", "--cwd", b.Worktree}
		_, stderr, code, err := m.opts.Herdr(ctx, args)
		switch {
		case err != nil:
			m.cmds.note("open worktree: run herdr: %v", err)
		case code != 0:
			m.cmds.note("open worktree: herdr exited %d: %s", code, strings.TrimSpace(stderr))
		default:
			m.cmds.note("opened %s in a herdr pane", b.Worktree)
		}
		return nil
	}
	shell := m.opts.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := m.opts.ShellCmd(shell, b.Worktree)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// shellDoneMsg reports that the shell started by o exited.
type shellDoneMsg struct{ err error }
