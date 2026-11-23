package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// EditFunc returns the command that runs editor on path; when the editor exits, the command
// reports done(err), where err is non-nil if the editor could not run or exited non-zero.
// Options.Edit injects it so tests skip the real exec.
type EditFunc func(editor, path string, done func(error) tea.Msg) tea.Cmd

// execEditor runs editor on path through sh (so $EDITOR may carry arguments) and suspends the
// TUI while it runs.
func execEditor(editor, path string, done func(error) tea.Msg) tea.Cmd {
	c := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	return tea.ExecProcess(c, func(err error) tea.Msg { return done(err) })
}

// changesTemplate is the comment file c opens; lines starting with # are dropped afterwards.
const changesTemplate = `
# Write the changes you want for %s above.
# Blank lines separate findings. Lines starting with # are ignored.
# Leave the file without comments to cancel.
`

// editedMsg reports that the editor on a request-changes comment file exited.
type editedMsg struct {
	task string
	path string
	err  error
}

// stripComments removes the template's # lines from the comment file and reports whether any
// text is left.
func stripComments(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			keep = append(keep, l)
		}
	}
	text := strings.Join(keep, "\n")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return false, err
	}
	return strings.TrimSpace(text) != "", nil
}
