package tui

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// maxActionWidth truncates the latest action, like `mill watch`.
const maxActionWidth = 80

// agentsView lists the running attempts.
type agentsView struct {
	now     func() time.Time
	running []store.Attempt
	at      time.Time
	cursor  int
}

// newAgentsView returns the Agents tab. now is the clock used for elapsed times when no
// snapshot has arrived yet.
func newAgentsView(now func() time.Time) view { return &agentsView{now: now} }

func (v *agentsView) SelectedTask() string {
	if v.cursor < 0 || v.cursor >= len(v.running) {
		return ""
	}
	return v.running[v.cursor].TaskID
}

func (v *agentsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case snapshot:
		v.running, v.at = msg.Running, msg.At
		v.cursor = min(v.cursor, max(0, len(v.running)-1))
	case tea.KeyMsg:
		switch msg.String() {
		case "up":
			v.cursor = max(0, v.cursor-1)
		case "down":
			v.cursor = min(max(0, len(v.running)-1), v.cursor+1)
		}
	}
	return nil
}

func (v *agentsView) View(width, height int) string {
	if len(v.running) == 0 {
		return "nothing running"
	}
	now := v.at
	if now.IsZero() {
		now = v.now()
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TASK\tATTEMPT\tROLE\tELAPSED\tTOKENS\tCOST\tLATEST ACTION")
	for i, a := range v.running {
		cost := fmt.Sprintf("$%.2f", a.Usage.CostUSD)
		if a.CostEstimated {
			cost = "~" + cost
		}
		mark := "  "
		if i == v.cursor {
			mark = "> "
		}
		fmt.Fprintf(tw, "%s%s\t#%d\t%s\t%s\t%s\t%s\t%s\n", mark, a.TaskID, a.N, a.Role,
			fmtElapsed(now.Sub(a.StartedAt)), fmtCount(a.Usage.Tokens(budget.CountBillable)), cost, latestAction(a))
	}
	tw.Flush()
	return clip(b.String(), width, height)
}

func latestAction(a store.Attempt) string {
	if a.LogPath == "" {
		return "-"
	}
	act, err := runner.LastAction(a.LogPath)
	switch {
	case err != nil:
		return "(stream unreadable: " + err.Error() + ")"
	case act == "":
		return "-"
	}
	if r := []rune(act); len(r) > maxActionWidth {
		act = string(r[:maxActionWidth-1]) + "…"
	}
	return act
}

func fmtElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if h := int(d.Hours()); h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// fmtCount formats n with thousands separators.
func fmtCount(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
