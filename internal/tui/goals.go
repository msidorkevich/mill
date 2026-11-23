package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/graph"
	"github.com/msidorkevich/mill/internal/store"
)

// goalsView lists goals and draws the selected goal's task graph.
type goalsView struct {
	cmds     *commands
	snap     snapshot
	cursor   int
	input    textinput.Model
	inputing bool
}

func newGoalsView(cmds *commands) *goalsView {
	in := textinput.New()
	in.Placeholder = "describe the goal"
	in.Prompt = "new goal: "
	return &goalsView{cmds: cmds, input: in}
}

func (v *goalsView) CapturingInput() bool { return v.inputing }

func (v *goalsView) selected() (store.Goal, bool) {
	if v.cursor < 0 || v.cursor >= len(v.snap.Goals) {
		return store.Goal{}, false
	}
	return v.snap.Goals[v.cursor], true
}

func (v *goalsView) SelectedTask() string { return "" }

func (v *goalsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case snapshot:
		v.snap = msg
		if v.cursor >= len(v.snap.Goals) {
			v.cursor = len(v.snap.Goals) - 1
		}
		if v.cursor < 0 {
			v.cursor = 0
		}
		return nil
	case tea.KeyMsg:
		if v.inputing {
			return v.updateInput(msg)
		}
		return v.updateKey(msg)
	}
	if v.inputing {
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	return nil
}

func (v *goalsView) updateInput(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		v.inputing = false
		v.input.Blur()
		v.input.Reset()
		return nil
	case tea.KeyEnter:
		text := strings.TrimSpace(v.input.Value())
		if text == "" {
			v.cmds.note("plan: type the goal first (esc cancels)")
			return nil
		}
		v.inputing = false
		v.input.Blur()
		v.input.Reset()
		return v.cmds.start("plan", text)
	}
	var cmd tea.Cmd
	v.input, cmd = v.input.Update(msg)
	return cmd
}

func (v *goalsView) updateKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "up":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down":
		if v.cursor < len(v.snap.Goals)-1 {
			v.cursor++
		}
	case "n":
		v.inputing = true
		return v.input.Focus()
	case "A", "enter":
		g, ok := v.selected()
		if !ok {
			v.cmds.note("approve: no goal selected")
			return nil
		}
		return v.cmds.start("plan", "approve", g.ID)
	case "R":
		g, ok := v.selected()
		if !ok {
			v.cmds.note("run: no goal selected")
			return nil
		}
		return v.cmds.start("run", g.ID)
	case "k":
		g, ok := v.selected()
		if !ok {
			v.cmds.note("keep: no goal selected")
			return nil
		}
		args := []string{"keep", g.ID, "--watch"}
		if e := v.cmds.latestRunning(isArgs(args...)); e != nil {
			v.cmds.interrupt(e, "keep "+g.ID)
			return nil
		}
		return v.cmds.start(args...)
	}
	return nil
}

func (v *goalsView) View(width, height int) string {
	var b strings.Builder
	if v.snap.Err != nil {
		fmt.Fprintf(&b, "✗ reading the store failed: %v\n", v.snap.Err)
	}
	if v.inputing {
		b.WriteString(v.input.View() + "\n")
	}
	if len(v.snap.Goals) == 0 {
		b.WriteString("no goals yet: press n to plan one\n")
		return clip(b.String(), width, height)
	}
	for i, g := range v.snap.Goals {
		mark := "  "
		if i == v.cursor {
			mark = "> "
		}
		fmt.Fprintf(&b, "%s%-5s %-9s %s\n", mark, g.ID, g.Status, g.Title)
	}
	b.WriteString("\n")
	g, _ := v.selected()
	b.WriteString(goalGraph(g, v.snap.Tasks[g.ID]))
	b.WriteString("\nn new plan  A/enter approve  R run  k keep (again: stop)\n")
	return clip(b.String(), width, height)
}

// goalGraph renders a goal's task graph with graph.Terminal.
func goalGraph(g store.Goal, tasks []goalTask) string {
	if len(tasks) == 0 {
		return fmt.Sprintf("%s has no tasks yet (the plan is %s): press A to approve it\n", g.ID, g.Status)
	}
	var gr graph.Graph
	for _, t := range tasks {
		_, l := store.SplitTaskID(t.ID)
		gr.Nodes = append(gr.Nodes, graph.Node{ID: l, Label: l, Status: t.Status})
		for _, d := range t.Deps {
			_, dl := store.SplitTaskID(d)
			gr.Edges = append(gr.Edges, graph.Edge{From: l, To: dl})
		}
	}
	var b strings.Builder
	if err := graph.Terminal(&b, gr); err != nil {
		return fmt.Sprintf("✗ drawing the graph of %s failed: %v\n", g.ID, err)
	}
	return b.String()
}

// clip keeps the first height lines of s, each cut to width; zero means no limit.
func clip(s string, width, height int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}
