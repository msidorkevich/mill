package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/store"
)

// view is one tab of the TUI.
type view interface {
	// Update handles a key or a snapshot message and may return a command.
	Update(msg tea.Msg) tea.Cmd
	// View renders the tab into at most width x height cells.
	View(width, height int) string
	// SelectedTask returns the full id of the task under the cursor, or "".
	SelectedTask() string
}

// inputCapturer is implemented by a view while it takes text input: the model then forwards
// every key to it instead of handling global keys.
type inputCapturer interface {
	CapturingInput() bool
}

// snapshot is what one poll read from the store. Views receive it as a message.
type snapshot struct {
	At      time.Time
	Goals   []store.Goal
	Tasks   map[string][]goalTask // by goal id, in plan order
	Review  []store.ReviewTask
	Running []store.Attempt
	Err     error // the poll failed; the other fields keep the previous values
}

// goalTask is a goal's task with its dependencies (full task ids).
type goalTask struct {
	store.Task
	Deps []string
}

// placeholder is a view that later tasks replace.
type placeholder struct{ title string }

func (p *placeholder) Update(tea.Msg) tea.Cmd { return nil }
func (p *placeholder) View(width, height int) string {
	return p.title + "\n\ncoming soon"
}
func (p *placeholder) SelectedTask() string { return "" }
