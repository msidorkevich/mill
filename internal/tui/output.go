package tui

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// procDoneMsg reports that a started command exited.
type procDoneMsg struct {
	id   int
	code int
	err  error
}

// lockedBuffer collects a command's streamed output; the command writes from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// entry is one line of history in the output panel: a note, or a started command.
type entry struct {
	id   int
	note string // set for notes; commands leave it empty
	args []string
	proc Proc
	out  *lockedBuffer
	done bool
	code int
	err  error
}

func (e *entry) running() bool { return e.proc != nil && !e.done }

// commands starts mill commands through the runner and keeps the output panel's history.
type commands struct {
	runner  CommandRunner
	entries []*entry
	nextID  int
}

// note adds a message to the output panel.
func (c *commands) note(format string, a ...any) {
	c.nextID++
	c.entries = append(c.entries, &entry{id: c.nextID, note: fmt.Sprintf(format, a...)})
}

// start starts `mill args...` and returns the command that waits for it.
func (c *commands) start(args ...string) tea.Cmd {
	out := &lockedBuffer{}
	p, err := c.runner.Start(args, out)
	if err != nil {
		c.note("✗ mill %s: could not start: %v", strings.Join(args, " "), err)
		return nil
	}
	c.nextID++
	e := &entry{id: c.nextID, args: args, proc: p, out: out}
	c.entries = append(c.entries, e)
	return func() tea.Msg {
		code, err := p.Wait()
		return procDoneMsg{id: e.id, code: code, err: err}
	}
}

// finish records a command's exit.
func (c *commands) finish(msg procDoneMsg) {
	for _, e := range c.entries {
		if e.id == msg.id {
			e.done, e.code, e.err = true, msg.code, msg.err
		}
	}
}

// latestRunning returns the most recent still-running command that match accepts, or nil.
func (c *commands) latestRunning(match func(args []string) bool) *entry {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if e := c.entries[i]; e.running() && match(e.args) {
			return e
		}
	}
	return nil
}

// interrupt sends SIGINT to e and says so in the panel.
func (c *commands) interrupt(e *entry, why string) {
	cmd := "mill " + strings.Join(e.args, " ")
	if err := e.proc.Interrupt(); err != nil {
		c.note("✗ %s: could not interrupt %s: %v", why, cmd, err)
		return
	}
	c.note("%s: sent interrupt to %s", why, cmd)
}

// isArgs returns a matcher for exactly these args.
func isArgs(want ...string) func([]string) bool {
	return func(args []string) bool { return slices.Equal(args, want) }
}

// render draws the panel's last height lines.
func (c *commands) render(width, height int) string {
	var lines []string
	for _, e := range c.entries {
		if e.proc == nil {
			lines = append(lines, e.note)
			continue
		}
		cmd := "mill " + strings.Join(e.args, " ")
		var status string
		switch {
		case !e.done:
			status = "▶ " + cmd + ": running"
		case e.err != nil:
			status = fmt.Sprintf("✗ %s: exit code %d: %v", cmd, e.code, e.err)
		case e.code != 0:
			status = fmt.Sprintf("✗ %s: failed with exit code %d", cmd, e.code)
		default:
			status = "✓ " + cmd + ": exit code 0"
		}
		lines = append(lines, status)
		for _, l := range strings.Split(strings.TrimRight(e.out.String(), "\n"), "\n") {
			if l != "" {
				lines = append(lines, "  "+l)
			}
		}
	}
	if len(lines) == 0 {
		lines = []string{"no commands yet"}
	}
	if height > 0 && len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// truncate cuts s to width cells (ANSI-aware); width <= 0 means no limit.
func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}
