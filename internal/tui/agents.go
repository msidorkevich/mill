package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// maxActionWidth truncates the latest action, like `mill watch`.
const maxActionWidth = 80

// agentsView lists the running attempts and the running stages mill runs itself.
type agentsView struct {
	now     func() time.Time
	paths   project.Paths
	running []store.Attempt
	stages  []store.RunningStage
	at      time.Time
	cursor  int
}

// newAgentsView returns the Agents tab. now is the clock used for elapsed times when no
// snapshot has arrived yet.
func newAgentsView(now func() time.Time, paths project.Paths) view {
	return &agentsView{now: now, paths: paths}
}

// rows is the number of listed rows: attempts first, then stages.
func (v *agentsView) rows() int { return len(v.running) + len(v.stages) }

func (v *agentsView) SelectedTask() string {
	switch {
	case v.cursor < 0 || v.cursor >= v.rows():
		return ""
	case v.cursor < len(v.running):
		return v.running[v.cursor].TaskID
	}
	return v.stages[v.cursor-len(v.running)].TaskID
}

func (v *agentsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case snapshot:
		v.running, v.stages, v.at = msg.Running, msg.Stages, msg.At
		v.cursor = min(v.cursor, max(0, v.rows()-1))
	case tea.KeyMsg:
		switch msg.String() {
		case "up":
			v.cursor = max(0, v.cursor-1)
		case "down":
			v.cursor = min(max(0, v.rows()-1), v.cursor+1)
		}
	}
	return nil
}

func (v *agentsView) View(width, height int) string {
	if v.rows() == 0 {
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
	for j, r := range v.stages {
		mark := "  "
		if len(v.running)+j == v.cursor {
			mark = "> "
		}
		stage := r.Phase
		if stage == "" {
			stage = "running"
		}
		fmt.Fprintf(tw, "%s%s\t#%d\t%s\t%s\t-\t-\t%s\n", mark, r.TaskID, r.N, stage,
			fmtElapsed(now.Sub(r.Since)), v.stageDetail(r))
	}
	tw.Flush()
	return clip(b.String(), width, height)
}

// stageDetail describes a stage mill runs itself. For a gate it is the newest gate log's last non-empty line and its path.
func (v *agentsView) stageDetail(r store.RunningStage) string {
	if r.Phase != store.PhaseGate {
		return "-"
	}
	dir := filepath.Join(v.paths.Runs, r.TaskID, fmt.Sprintf("a%d", r.N))
	logs, _ := filepath.Glob(filepath.Join(dir, "gate-*.log"))
	if len(logs) == 0 {
		return "(no gate log yet)"
	}
	sort.Strings(logs)
	newest, newestMod := "", time.Time{}
	for _, l := range logs {
		if st, err := os.Stat(l); err == nil && !st.ModTime().Before(newestMod) {
			newest, newestMod = l, st.ModTime()
		}
	}
	if newest == "" {
		return "(gate log unreadable: " + logs[0] + ")"
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		return "(gate log unreadable: " + err.Error() + ")"
	}
	last := ""
	for _, line := range strings.Split(string(data), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			last = l
		}
	}
	if r := []rune(last); len(r) > maxActionWidth {
		last = string(r[:maxActionWidth-1]) + "…"
	}
	if last == "" {
		return newest
	}
	return last + " (" + newest + ")"
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
