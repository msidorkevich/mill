// Package tui implements `mill tui`, an interactive terminal UI over the store. It only reads
// the store; every change goes through a mill command started by a CommandRunner.
package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// Store is the read-only part of the store the TUI polls. *store.Store satisfies it.
type Store interface {
	ListGoals(ctx context.Context) ([]store.Goal, error)
	GoalTasks(ctx context.Context, goalID string) ([]store.Task, error)
	TaskDeps(ctx context.Context, taskID string) ([]string, error)
	ReviewTasks(ctx context.Context) ([]store.ReviewTask, error)
	RunningAttempts(ctx context.Context) ([]store.Attempt, error)
}

// Options build a Model.
type Options struct {
	Store    Store
	Paths    project.Paths
	Runner   CommandRunner
	Now      func() time.Time             // defaults to time.Now
	Getenv   func(string) string          // defaults to os.Getenv (used by later views)
	LookPath func(string) (string, error) // defaults to exec.LookPath (used by later views)
}

// pollInterval is how often the model re-reads the store.
const pollInterval = time.Second

// tickMsg triggers a store poll.
type tickMsg time.Time

// Model is the Bubble Tea model of `mill tui`.
type Model struct {
	opts     Options
	cmds     *commands
	views    []view
	names    []string
	active   int
	help     bool
	width    int
	height   int
	snap     snapshot
	quitting bool
}

// New returns a Model. Call Init (tea.NewProgram does) to start polling.
func New(opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	cmds := &commands{runner: opts.Runner}
	return &Model{
		opts:   opts,
		cmds:   cmds,
		views:  []view{newGoalsView(cmds), newReviewView(), newDetailView(), newAgentsView()},
		names:  []string{"Goals", "Review", "Task detail", "Agents"},
		width:  100,
		height: 30,
	}
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init polls at once and starts the tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return tickMsg(m.opts.Now()) }, tick())
}

// poll reads the store (read methods only) and hands the snapshot to every view.
func (m *Model) poll() tea.Cmd {
	ctx := context.Background()
	next := m.snap
	next.At = m.opts.Now()
	next.Err = nil
	if err := m.read(ctx, &next); err != nil {
		next = m.snap
		next.Err = err
	}
	m.snap = next
	var batch []tea.Cmd
	for _, v := range m.views {
		batch = append(batch, v.Update(next))
	}
	return tea.Batch(batch...)
}

func (m *Model) read(ctx context.Context, s *snapshot) error {
	goals, err := m.opts.Store.ListGoals(ctx)
	if err != nil {
		return err
	}
	tasks := map[string][]goalTask{}
	for _, g := range goals {
		ts, err := m.opts.Store.GoalTasks(ctx, g.ID)
		if err != nil {
			return err
		}
		for _, t := range ts {
			deps, err := m.opts.Store.TaskDeps(ctx, t.ID)
			if err != nil {
				return err
			}
			tasks[g.ID] = append(tasks[g.ID], goalTask{Task: t, Deps: deps})
		}
	}
	review, err := m.opts.Store.ReviewTasks(ctx)
	if err != nil {
		return err
	}
	running, err := m.opts.Store.RunningAttempts(ctx)
	if err != nil {
		return err
	}
	s.Goals, s.Tasks, s.Review, s.Running = goals, tasks, review, running
	return nil
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, tea.Batch(m.poll(), tick())
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case procDoneMsg:
		m.cmds.finish(msg)
		return m, m.poll()
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, m.views[m.active].Update(msg)
}

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyCtrlC {
		m.quitting = true
		return tea.Quit
	}
	v := m.views[m.active]
	if c, ok := v.(inputCapturer); ok && c.CapturingInput() {
		return v.Update(msg)
	}
	switch msg.String() {
	case "q":
		m.quitting = true
		return tea.Quit
	case "?":
		m.help = !m.help
		return nil
	case "tab":
		m.active = (m.active + 1) % len(m.views)
		return nil
	case "shift+tab":
		m.active = (m.active + len(m.views) - 1) % len(m.views)
		return nil
	case "p":
		m.pause()
		return nil
	case "o":
		return m.openWorktree(v.SelectedTask())
	}
	return v.Update(msg)
}

// pause interrupts the most recent still-running run or keep process this TUI started.
func (m *Model) pause() {
	e := m.cmds.latestRunning(func(args []string) bool {
		return len(args) > 0 && (args[0] == "run" || args[0] == "keep")
	})
	if e == nil {
		m.cmds.note("nothing to pause")
		return
	}
	m.cmds.interrupt(e, "pause")
}

var (
	activeTab = lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1)
	otherTab  = lipgloss.NewStyle().Padding(0, 1)
	dim       = lipgloss.NewStyle().Faint(true)
)

const helpText = `Keys
  tab / shift-tab  switch tabs
  ↑ / ↓            move the selection
  enter / esc      open or confirm / cancel
  n                plan a new goal (Goals)
  A                approve the selected plan (Goals)
  R                run the selected goal (Goals)
  k                keep the goal mergeable; press again to stop (Goals)
  a                approve
  c                request changes
  x                reject
  r                retry
  l                log
  p                pause: interrupt the latest run or keep started here
  o                open worktree
  s                skip (not available: mill has no skip)
  ?                toggle this help
  q                quit`

// View renders the TUI.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	var tabs []string
	for i, n := range m.names {
		if i == m.active {
			tabs = append(tabs, activeTab.Render(n))
		} else {
			tabs = append(tabs, otherTab.Render(n))
		}
	}
	panelH := max(3, m.height/4)
	bodyH := max(1, m.height-panelH-4)
	body := helpText
	if !m.help {
		body = m.views[m.active].View(m.width, bodyH)
	}
	body = clip(body, m.width, bodyH)
	if n := strings.Count(body, "\n") + 1; n < bodyH {
		body += strings.Repeat("\n", bodyH-n)
	}
	var b strings.Builder
	b.WriteString(strings.Join(tabs, " ") + "\n")
	b.WriteString(body + "\n")
	b.WriteString(dim.Render(truncate("── output "+strings.Repeat("─", max(0, m.width-10)), m.width)) + "\n")
	b.WriteString(m.cmds.render(m.width, panelH) + "\n")
	b.WriteString(dim.Render("? help  q quit"))
	return b.String()
}
