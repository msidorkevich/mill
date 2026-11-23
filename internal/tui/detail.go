package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// backToReviewMsg asks the model to show the Review view again.
type backToReviewMsg struct{}

// taskShow is the part of `mill task show --json` the detail view shows.
type taskShow struct {
	Task     store.Task      `json:"task"`
	Attempts []store.Attempt `json:"attempts"`
	Usage    budget.Usage    `json:"usage"`
}

// reviewShow is the part of `mill review show --json` the detail view shows.
type reviewShow struct {
	Verdict  *prompt.VerifierOutput `json:"verdict"`
	Gate     *gate.Result           `json:"gate"`
	Approval string                 `json:"approval"`
	Waiting  string                 `json:"waiting"`
}

// detailMsg carries what one refresh read through the CLI. Each *Err holds the failure (with
// the CLI's stderr) of its capture.
type detailMsg struct {
	seq       int
	task      string
	show      taskShow
	showErr   string
	review    reviewShow
	reviewErr string
	diff      string
	diffErr   string
}

// detailView shows one task: spec, review state, diff and optionally its agent's log.
type detailView struct {
	acts   *taskActions
	store  Store
	task   string
	seq    int
	data   *detailMsg // nil until the first refresh of task arrives
	vp     viewport.Model
	logOn  bool
	logs   []string
	logErr string
}

func newDetailView(acts *taskActions, s Store) *detailView {
	return &detailView{acts: acts, store: s, vp: viewport.New(80, 10)}
}

func (v *detailView) CapturingInput() bool { return v.acts.capturing() }

func (v *detailView) SelectedTask() string { return v.task }

func (v *detailView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case openDetailMsg:
		if msg.task != v.task {
			v.task, v.data, v.logs, v.logErr = msg.task, nil, nil, ""
			v.vp.SetContent("")
			v.vp.GotoTop()
		}
		if v.logOn {
			v.readLog()
		}
		return v.refresh()
	case snapshot:
		if v.logOn {
			v.readLog()
		}
		return nil
	case procDoneMsg:
		return v.refresh()
	case detailMsg:
		if msg.seq == v.seq && msg.task == v.task {
			v.data = &msg
			v.vp.SetContent(msg.diff)
		}
		return nil
	case tea.KeyMsg:
		if v.acts.capturing() {
			return v.acts.updateInput(msg)
		}
		if cmd, ok := v.acts.key(v.task, msg); ok {
			return cmd
		}
		switch msg.String() {
		case "esc":
			return func() tea.Msg { return backToReviewMsg{} }
		case "enter":
			return v.refresh()
		case "l":
			v.logOn = !v.logOn
			if v.logOn {
				v.readLog()
			}
		case "up":
			v.vp.ScrollUp(1)
		case "down":
			v.vp.ScrollDown(1)
		case "pgup":
			v.vp.PageUp()
		case "pgdown":
			v.vp.PageDown()
		}
		return nil
	}
	cmd, _ := v.acts.handle(msg)
	return cmd
}

// refresh returns the command that reads the task through the CLI.
func (v *detailView) refresh() tea.Cmd {
	if v.task == "" {
		return nil
	}
	v.seq++
	seq, id, r := v.seq, v.task, v.acts.cmds.runner
	return func() tea.Msg {
		ctx := context.Background()
		m := detailMsg{seq: seq, task: id}
		capture := func(dst any, args ...string) (string, string) {
			stdout, stderr, code, err := r.Capture(ctx, args)
			if msg := captureFailure(args, stderr, code, err); msg != "" {
				return "", msg
			}
			if dst != nil {
				if err := json.Unmarshal([]byte(stdout), dst); err != nil {
					return "", fmt.Sprintf("mill %s printed invalid JSON: %v", strings.Join(args, " "), err)
				}
			}
			return stdout, ""
		}
		_, m.showErr = capture(&m.show, "task", "show", id, "--json")
		_, m.reviewErr = capture(&m.review, "review", "show", id, "--json")
		m.diff, m.diffErr = capture(nil, "review", "show", id, "--diff")
		return m
	}
}

// readLog re-reads the actions of the task's latest attempt.
func (v *detailView) readLog() {
	v.logs, v.logErr = nil, ""
	if v.task == "" {
		return
	}
	atts, err := v.store.Attempts(context.Background(), v.task)
	if err != nil {
		v.logErr = fmt.Sprintf("reading the attempts of %s failed: %v", v.task, err)
		return
	}
	if len(atts) == 0 {
		v.logErr = v.task + " has no attempts yet"
		return
	}
	last := atts[len(atts)-1]
	if last.LogPath == "" {
		v.logErr = fmt.Sprintf("attempt %d of %s has no log yet", last.N, v.task)
		return
	}
	if v.logs, err = runner.Actions(last.LogPath); err != nil {
		v.logErr = err.Error()
	}
}

func (v *detailView) View(width, height int) string {
	if v.task == "" {
		return "no task selected: pick one in the Review tab and press enter"
	}
	var b strings.Builder
	b.WriteString(v.acts.inputView())
	d := v.data
	if d == nil {
		fmt.Fprintf(&b, "%s: loading…\n", v.task)
		return clip(b.String(), width, height)
	}
	if d.showErr != "" {
		fmt.Fprintf(&b, "%s\n✗ %s\n", v.task, d.showErr)
	} else {
		t := d.show.Task
		fmt.Fprintf(&b, "%s  %s  %s\n", t.ID, t.Status, t.Title)
		fmt.Fprintf(&b, "attempts: %d  cost: $%.2f\n", len(d.show.Attempts), d.show.Usage.CostUSD)
		b.WriteString("\nSpec:\n")
		for _, l := range strings.Split(strings.TrimRight(t.Spec.Spec, "\n"), "\n") {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("Acceptance:\n")
		for _, a := range t.Acceptance {
			b.WriteString("  - " + a + "\n")
		}
	}
	b.WriteString("\n")
	if d.reviewErr != "" {
		fmt.Fprintf(&b, "✗ %s\n", d.reviewErr)
	} else {
		writeReview(&b, d.review)
	}
	if v.logOn {
		fmt.Fprintf(&b, "\nLog of the latest attempt (l hides):\n")
		if v.logErr != "" {
			fmt.Fprintf(&b, "  ✗ %s\n", v.logErr)
		} else if len(v.logs) == 0 {
			b.WriteString("  no actions yet\n")
		}
		logs := v.logs
		if n := max(5, height/3); len(logs) > n {
			logs = logs[len(logs)-n:]
		}
		for _, l := range logs {
			b.WriteString("  " + l + "\n")
		}
	}
	if d.diffErr != "" {
		fmt.Fprintf(&b, "\nDiff:\n✗ %s\n", d.diffErr)
	} else {
		b.WriteString("\nDiff (↑/↓ pgup/pgdn scroll):\n")
	}
	keys := "enter refresh  esc back  a approve  c changes  x reject  r retry  l log"
	used := strings.Count(b.String(), "\n") + 1
	if d.diffErr == "" {
		v.vp.Width = max(1, width)
		v.vp.Height = max(3, height-used)
		v.vp.SetYOffset(v.vp.YOffset) // clamp after a resize
		b.WriteString(v.vp.View() + "\n")
	}
	b.WriteString(keys)
	return clip(b.String(), width, height)
}

// writeReview renders the last verdict, its findings and the last gate result.
func writeReview(b *strings.Builder, r reviewShow) {
	if r.Approval != "" {
		fmt.Fprintf(b, "approval: %s\n", r.Approval)
	}
	if r.Waiting != "" {
		fmt.Fprintf(b, "waiting: %s\n", r.Waiting)
	}
	if r.Verdict == nil {
		b.WriteString("last verdict: none\n")
	} else {
		fmt.Fprintf(b, "last verdict: %s: %s\n", r.Verdict.Verdict, oneLine(r.Verdict.Summary))
		for _, f := range r.Verdict.Findings {
			b.WriteString("  - " + oneLine(f.String()) + "\n")
		}
	}
	if r.Gate == nil {
		b.WriteString("last gate: none\n")
		return
	}
	status := "passed"
	if !r.Gate.Passed {
		status = "failed"
	}
	if r.Gate.Reason != "" {
		status += ": " + oneLine(r.Gate.Reason)
	}
	fmt.Fprintf(b, "last gate: %s\n", status)
	for _, c := range r.Gate.Commands {
		mark := "✓"
		if !c.Passed() {
			mark = "✗"
		}
		line := fmt.Sprintf("  %s %s (exit %d)", mark, c.Cmd, c.Exit)
		if c.Error != "" {
			line += ": " + oneLine(c.Error)
		}
		b.WriteString(line + "\n")
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
