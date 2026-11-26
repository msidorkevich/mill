package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

const watchUsage = "Usage: mill watch [<goal>|<task>] [--once] [--interval <duration>]"

// maxActionWidth cuts the latest action so a line stays readable on a normal terminal.
const maxActionWidth = 80

func runWatch(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "watch")
	once := fs.Bool("once", false, "print once and exit")
	interval := fs.Duration("interval", 2*time.Second, "redraw interval")
	var sel string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sel, args = args[0], args[1:]
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *interval <= 0 {
		fmt.Fprintf(env.Stderr, "mill watch: --interval must be positive\n%s\n", watchUsage)
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill watch: %v\n", err)
		return ExitError
	}
	paths, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	if sel != "" {
		if _, _, err := watchScope(ctx, s, sel); err != nil {
			return fail(err)
		}
	}

	if *once || !isTerminal(env.Stdout) {
		if err := renderWatch(ctx, env.Stdout, paths, s, sel, time.Now()); err != nil {
			return fail(err)
		}
		return ExitOK
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		fmt.Fprint(env.Stdout, "\x1b[H\x1b[2J")
		if err := renderWatch(ctx, env.Stdout, paths, s, sel, time.Now()); err != nil && ctx.Err() == nil {
			return fail(err)
		}
		select {
		case <-ctx.Done():
			return ExitOK
		case <-tick.C:
		}
	}
}

// isTerminal reports whether w is a character device such as a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// watchScope resolves sel (a goal or task id; "" = everything) to a label for the counts line
// and the statuses of the tasks in scope.
func watchScope(ctx context.Context, s *store.Store, sel string) (label string, statuses map[string]string, err error) {
	statuses = map[string]string{}
	if sel == "" {
		sums, err := s.TaskSummaries(ctx, "")
		if err != nil {
			return "", nil, err
		}
		for _, t := range sums {
			statuses[t.ID] = t.Status
		}
		return "all tasks", statuses, nil
	}
	if !strings.Contains(sel, "/") {
		if _, gerr := s.GetGoal(ctx, sel); gerr == nil {
			tasks, err := s.GoalTasks(ctx, sel)
			if err != nil {
				return "", nil, err
			}
			for _, t := range tasks {
				statuses[t.ID] = t.Status
			}
			return "goal " + sel, statuses, nil
		} else if !errors.Is(gerr, store.ErrNotFound) {
			return "", nil, gerr
		}
	}
	t, err := s.GetTask(ctx, sel)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, fmt.Errorf("no goal or task %q: list goals with `mill goal list`, tasks with `mill task list`", sel)
	}
	if err != nil {
		return "", nil, err
	}
	statuses[t.ID] = t.Status
	return "task " + sel, statuses, nil
}

func renderWatch(ctx context.Context, w io.Writer, paths project.Paths, s *store.Store, sel string, now time.Time) error {
	label, statuses, err := watchScope(ctx, s, sel)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, st := range statuses {
		counts[st]++
	}
	var parts []string
	for _, st := range store.TaskStatuses {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no tasks"}
	}
	fmt.Fprintf(w, "%s: %s\n\n", label, strings.Join(parts, ", "))

	all, err := s.RunningAttempts(ctx)
	if err != nil {
		return err
	}
	var running []store.Attempt
	for _, a := range all {
		if _, ok := statuses[a.TaskID]; ok {
			running = append(running, a)
		}
	}
	allStages, err := s.RunningStages(ctx)
	if err != nil {
		return err
	}
	var stages []store.RunningStage
	for _, r := range allStages {
		if _, ok := statuses[r.TaskID]; ok {
			stages = append(stages, r)
		}
	}
	shown := map[string]bool{}
	for _, a := range running {
		shown[a.TaskID] = true
	}
	for _, r := range stages {
		shown[r.TaskID] = true
	}
	allClaims, err := s.ClaimedTasks(ctx)
	if err != nil {
		return err
	}
	var claims []store.TaskClaim
	for id, c := range allClaims {
		if _, ok := statuses[id]; ok && !shown[id] {
			claims = append(claims, c)
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].TaskID < claims[j].TaskID })
	if len(running) == 0 && len(stages) == 0 && len(claims) == 0 {
		fmt.Fprintln(w, "nothing running")
		return nil
	}
	if len(running) > 0 || len(stages) > 0 {
		if err := renderRunning(w, paths, running, stages, now); err != nil {
			return err
		}
	}
	if len(claims) > 0 {
		if len(running) > 0 || len(stages) > 0 {
			fmt.Fprintln(w)
		}
		renderClaims(w, claims, now)
	}
	return nil
}

// renderClaims lists tasks a mill command holds a claim on without running them, with what
// the holder says it is doing and how long it has held the claim.
func renderClaims(w io.Writer, claims []store.TaskClaim, now time.Time) {
	for _, c := range claims {
		activity := c.Activity
		if activity == "" {
			activity = "-"
		}
		if r := []rune(activity); len(r) > maxActionWidth {
			activity = string(r[:maxActionWidth-1]) + "…"
		}
		fmt.Fprintf(w, "%s  claimed by %s (pid %d): %s  held %s\n", c.TaskID, c.Command, c.PID, activity,
			fmtElapsed(now.Sub(c.ClaimedAt)))
	}
}

func renderRunning(w io.Writer, paths project.Paths, running []store.Attempt, stages []store.RunningStage, now time.Time) error {
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tATTEMPT\tROLE\tELAPSED\tTOKENS\tCOST\tLATEST ACTION")
	for _, a := range running {
		cost := fmt.Sprintf("$%.2f", a.Usage.CostUSD)
		if a.CostEstimated {
			cost = "~" + cost
		}
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\t%s\t%s\n", a.TaskID, a.N, a.Role,
			fmtElapsed(now.Sub(a.StartedAt)), fmtInt(a.Usage.Tokens(budget.CountBillable)), cost, latestAction(a))
	}
	for _, r := range stages {
		stage := r.Phase
		if stage == "" {
			stage = "running"
		}
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t-\t-\t%s\n", r.TaskID, r.N, stage,
			fmtElapsed(now.Sub(r.Since)), stageDetail(paths, r))
	}
	return tw.Flush()
}

// stageDetail describes a stage mill runs itself. For a gate it is the newest gate log's last
// non-empty line and its path; a task waiting for a gate slot (stage "waiting for gate") has
// no detail.
func stageDetail(paths project.Paths, r store.RunningStage) string {
	if r.Phase != store.PhaseGate {
		return "-"
	}
	dir := filepath.Join(paths.Runs, r.TaskID, fmt.Sprintf("a%d", r.N))
	logs, _ := filepath.Glob(filepath.Join(dir, "gate-*.log"))
	if len(logs) == 0 {
		return "(no gate log yet)"
	}
	newest, newestMod := "", time.Time{}
	sort.Strings(logs)
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
