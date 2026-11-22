package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"mill/internal/budget"
	"mill/internal/runner"
	"mill/internal/store"
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
	_, _, s, err := openProject(ctx, env)
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
		if err := renderWatch(ctx, env.Stdout, s, sel, time.Now()); err != nil {
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
		if err := renderWatch(ctx, env.Stdout, s, sel, time.Now()); err != nil && ctx.Err() == nil {
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

func renderWatch(ctx context.Context, w io.Writer, s *store.Store, sel string, now time.Time) error {
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
	if len(running) == 0 {
		fmt.Fprintln(w, "nothing running")
		return nil
	}
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
	return tw.Flush()
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
