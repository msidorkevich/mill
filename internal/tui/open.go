package tui

import tea "github.com/charmbracelet/bubbletea"

// openWorktree opens a shell in the task's worktree. Not implemented yet: it only says so.
func (m *Model) openWorktree(taskID string) tea.Cmd {
	m.cmds.note("open worktree: not available yet")
	return nil
}
