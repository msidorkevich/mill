package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/planner"
	"github.com/msidorkevich/mill/internal/store"
)

func runPlan(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		planUsage(env.Stderr)
		return ExitUsage
	}
	switch args[0] {
	case "-h", "--help":
		planUsage(env.Stdout)
		return ExitOK
	case "show":
		return runPlanShow(ctx, env, args[1:])
	case "edit":
		return runPlanEdit(ctx, env, args[1:])
	case "approve":
		return runPlanApprove(ctx, env, args[1:])
	}
	return runPlanCreate(ctx, env, args)
}

func planUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mill plan \"<goal>\"                        let the planner agent write a plan; create a draft goal")
	fmt.Fprintln(w, "  mill plan --file plan.json                 create a draft goal from a written plan")
	fmt.Fprintln(w, "  mill plan show <goal> [--json]             show a goal's plan")
	fmt.Fprintln(w, "  mill plan edit <goal> [--file plan.json]   replace a draft plan (opens $EDITOR without --file)")
	fmt.Fprintln(w, "  mill plan approve <goal>                   approve a draft plan and create its tasks")
}

// problems prints every problem in err, one per line.
func problems(w io.Writer, prefix string, err error) {
	var list []error
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		list = j.Unwrap()
	} else {
		list = []error{err}
	}
	for _, e := range list {
		fmt.Fprintf(w, "%s  - %v\n", prefix, e)
	}
}

// loadPlan parses and validates plan JSON. A non-nil err means the plan is unusable.
func loadPlan(data []byte) (plan.Plan, error) {
	p, err := plan.Parse(data)
	if err != nil {
		return p, err
	}
	if err := p.Validate(plan.Options{}); err != nil {
		return p, err
	}
	return p, nil
}

func absPath(env Env, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(env.Dir, p)
}

func printWarnings(env Env, p plan.Plan) {
	for _, w := range p.Warnings() {
		fmt.Fprintf(env.Stdout, "warning: %s\n", w)
	}
}

func runPlanCreate(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "plan")
	file := fs.String("file", "", "plan file (JSON)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flagErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill plan: %v\n", err)
		return ExitError
	}
	if *file == "" {
		if fs.NArg() == 0 {
			planUsage(env.Stderr)
			return ExitUsage
		}
		return runPlanGoal(ctx, env, strings.Join(fs.Args(), " "))
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "mill plan: unexpected arguments: %s (use either --file or a goal text)\n", strings.Join(fs.Args(), " "))
		return ExitUsage
	}
	data, err := os.ReadFile(absPath(env, *file))
	if err != nil {
		return fail(err)
	}
	p, err := loadPlan(data)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill plan: %s is not a valid plan:\n", *file)
		problems(env.Stderr, "", err)
		fmt.Fprintln(env.Stderr, "Fix the problems and run `mill plan --file` again. No goal was created.")
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	g, err := createDraftGoal(ctx, s, p)
	if err != nil {
		return fail(err)
	}
	printDraftGoal(env, g, p)
	return ExitOK
}

// createDraftGoal stores a valid plan as a new draft goal.
func createDraftGoal(ctx context.Context, s *store.Store, p plan.Plan) (store.Goal, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return store.Goal{}, err
	}
	return s.CreateGoal(ctx, p.Goal, raw)
}

func printDraftGoal(env Env, g store.Goal, p plan.Plan) {
	fmt.Fprintf(env.Stdout, "Created goal %s (draft): %s\n", g.ID, p.Goal)
	fmt.Fprintf(env.Stdout, "%d tasks\n", len(p.Tasks))
	printWarnings(env, p)
	fmt.Fprintf(env.Stdout, "\nNext: review with `mill plan show %s`, then `mill plan approve %s`.\n", g.ID, g.ID)
}

// runPlanGoal asks the planner agent for a plan of goal and creates a draft goal from it.
func runPlanGoal(ctx context.Context, env Env, goal string) int {
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill plan: %v\n", err)
		return ExitError
	}
	if strings.TrimSpace(goal) == "" {
		return fail(errors.New("the goal text is empty: describe what you want built"))
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	p, err := newPipeline(env, "plan", paths, cfg, s, env.Stdout)
	if err != nil {
		return fail(err)
	}
	logDir, err := newPlanLogDir(paths.Runs)
	if err != nil {
		return fail(err)
	}
	allow := append(append(append([]string{}, host.DefaultEnvAllow...), cfg.EnvAllow...), p.AgentEnvAllow...)

	fmt.Fprintf(env.Stdout, "Planning (model %s, logs in %s)...\n", cfg.Model("M"), logDir)
	res, err := planner.Generate(ctx, planner.Options{
		Runner: p.Runner, Goal: goal, Root: paths.Root, CheckCmd: cfg.CheckCmd,
		Model: cfg.Model("M"), Budget: cfg.AttemptBudget("M"), Count: cfg.Budget.Count,
		LogDir: logDir, ProtectedPaths: []string{paths.MillDir}, Env: host.AllowEnv(os.Environ(), allow),
		Timeout: cfg.Runner.AttemptTimeout.Duration,
	})
	cost := fmt.Sprintf("planning cost: %s tokens (billable), $%.2f in %d agent call(s)",
		fmtInt(res.Usage.Tokens(budget.CountBillable)), res.Usage.CostUSD, res.Calls)
	var invalid *planner.InvalidPlanError
	if errors.As(err, &invalid) {
		path := filepath.Join(logDir, "plan.json")
		if werr := os.WriteFile(path, append([]byte(res.Raw), '\n'), 0o644); werr != nil {
			fmt.Fprintf(env.Stderr, "mill plan: could not save the last plan: %v\n", werr)
			path = ""
		}
		fmt.Fprintln(env.Stderr, "mill plan: the planner's plan is still invalid after one retry:")
		for _, pr := range invalid.Problems {
			fmt.Fprintf(env.Stderr, "  - %s\n", pr)
		}
		if path != "" {
			fmt.Fprintf(env.Stderr, "The last plan is saved in %s\nFix it and run `mill plan --file %s`.\n", path, path)
		}
		fmt.Fprintf(env.Stderr, "No goal was created (%s).\n", cost)
		return ExitError
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill plan: %v\nNo goal was created (%s); logs are in %s.\n", err, cost, logDir)
		return ExitError
	}
	g, err := createDraftGoal(ctx, s, res.Plan)
	if err != nil {
		return fail(err)
	}
	printDraftGoal(env, g, res.Plan)
	fmt.Fprintf(env.Stdout, "%s\n", strings.ToUpper(cost[:1])+cost[1:])
	return ExitOK
}

// newPlanLogDir creates .mill/runs/plans/<timestamp>/, adding a suffix if it exists.
func newPlanLogDir(runs string) (string, error) {
	base := filepath.Join(runs, "plans", time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return "", err
	}
	dir := base
	for i := 2; ; i++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}
}

// goalArg parses "<goal> [flags]" with the id before or after the flags.
func goalArg(env Env, name, usage string, fs interface {
	Parse([]string) error
	NArg() int
	Arg(int) string
}, args []string) (string, int, bool) {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flagErrHelp) {
			return "", ExitOK, false
		}
		return "", ExitUsage, false
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() > 0 || id == "" {
		fmt.Fprintln(env.Stderr, "Usage: "+usage)
		return "", ExitUsage, false
	}
	return id, 0, true
}

type planTask struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	Size       string   `json:"size"`
	DependsOn  []string `json:"depends_on"`
	Owns       []string `json:"owns"`
	Checks     []string `json:"checks"`
	Acceptance []string `json:"acceptance"`
	Status     string   `json:"status,omitempty"`
	Phase      string   `json:"phase,omitempty"`
}

func runPlanShow(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "plan show")
	asJSON := fs.Bool("json", false, "print one JSON document")
	id, code, ok := goalArg(env, "plan show", "mill plan show <goal> [--json]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill plan show: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	g, err := s.GetGoal(ctx, id)
	if err != nil {
		return fail(err)
	}
	p, err := plan.Parse(g.Plan)
	if err != nil {
		return fail(fmt.Errorf("stored plan of %s is unreadable: %w", id, err))
	}
	order, err := p.Order()
	if err != nil {
		return fail(fmt.Errorf("plan of %s has no valid order: %w; fix it with `mill plan edit %s`", id, err, id))
	}
	byID := map[string]int{}
	for i, t := range p.Tasks {
		byID[t.ID] = i
	}
	var tasks []planTask
	for _, tid := range order {
		t := p.Tasks[byID[tid]]
		pt := planTask{ID: t.ID, Title: t.Title, Kind: t.Kind, Size: t.Size, DependsOn: nz(t.DependsOn),
			Owns: nz(t.Owns), Checks: nz(t.Checks), Acceptance: nz(t.Acceptance)}
		if g.Status != store.GoalDraft {
			st, err := s.GetTask(ctx, store.TaskID(g.ID, t.ID))
			if err != nil {
				return fail(err)
			}
			pt.Status, pt.Phase = st.Status, st.Phase
		}
		tasks = append(tasks, pt)
	}
	warnings := p.Warnings()
	if warnings == nil {
		warnings = []string{}
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema   string     `json:"schema"`
			Goal     store.Goal `json:"goal"`
			Order    []planTask `json:"order"`
			Warnings []string   `json:"warnings"`
		}{"mill/plan@1", g, tasks, warnings}); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	w := env.Stdout
	fmt.Fprintf(w, "%s: %s\n", g.ID, g.Title)
	fmt.Fprintf(w, "  status    %s\n", g.Status)
	if g.Reason != "" {
		fmt.Fprintf(w, "  reason    %s\n", g.Reason)
	}
	if len(p.Assumptions) > 0 {
		fmt.Fprintln(w, "\nAssumptions:")
		for _, a := range p.Assumptions {
			fmt.Fprintf(w, "  - %s\n", a)
		}
	}
	fmt.Fprintf(w, "\nTasks (%d, in dependency order):\n", len(tasks))
	checksLabel := "checks"
	if g.Status == store.GoalDraft {
		checksLabel = "checks (untrusted until approved)"
	}
	for i, t := range tasks {
		fmt.Fprintf(w, "\n%d. %s: %s\n", i+1, t.ID, t.Title)
		fmt.Fprintf(w, "   kind        %s, size %s\n", t.Kind, t.Size)
		if t.Status != "" {
			st := t.Status
			if t.Phase != "" {
				st += " (" + t.Phase + ")"
			}
			fmt.Fprintf(w, "   status      %s\n", st)
		}
		fmt.Fprintf(w, "   depends_on  %s\n", listOrNone(t.DependsOn))
		fmt.Fprintf(w, "   owns        %s\n", listOrNone(t.Owns))
		fmt.Fprintf(w, "   %s\n", checksLabel)
		for _, c := range t.Checks {
			fmt.Fprintf(w, "     $ %s\n", c)
		}
		if len(t.Checks) == 0 {
			fmt.Fprintln(w, "     (none)")
		}
		fmt.Fprintln(w, "   acceptance")
		for _, a := range t.Acceptance {
			fmt.Fprintf(w, "     - %s\n", a)
		}
	}
	if len(warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, x := range warnings {
			fmt.Fprintf(w, "  - %s\n", x)
		}
	}
	if g.Status == store.GoalDraft {
		fmt.Fprintf(w, "\nNext: `mill plan edit %s` to change it, or `mill plan approve %s`.\n", g.ID, g.ID)
	}
	return ExitOK
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func runPlanEdit(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "plan edit")
	file := fs.String("file", "", "plan file (JSON) that replaces the plan")
	id, code, ok := goalArg(env, "plan edit", "mill plan edit <goal> [--file plan.json]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill plan edit: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	g, err := s.GetGoal(ctx, id)
	if err != nil {
		return fail(err)
	}
	if g.Status != store.GoalDraft {
		return fail(fmt.Errorf("goal %s is %s: the plan is approved and can no longer be edited", id, g.Status))
	}

	path := *file
	temp := false
	if path == "" {
		cur, err := plan.Parse(g.Plan)
		if err != nil {
			return fail(fmt.Errorf("stored plan of %s is unreadable: %w", id, err))
		}
		data, err := json.MarshalIndent(cur, "", "  ")
		if err != nil {
			return fail(err)
		}
		path, err = editTemp(ctx, env, "mill-plan-"+id, data)
		if err != nil {
			return fail(fmt.Errorf("%w; the plan was not changed", err))
		}
		temp = true
	} else {
		path = absPath(env, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	p, err := loadPlan(data)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill plan edit: the plan in %s is not valid; %s is unchanged:\n", path, id)
		problems(env.Stderr, "", err)
		if temp {
			fmt.Fprintf(env.Stderr, "Your edit is kept in %s\nFix it, then run `mill plan edit %s --file %s`.\n", path, id, path)
		}
		return ExitError
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fail(err)
	}
	if err := s.UpdateGoalPlan(ctx, id, raw); err != nil {
		return fail(err)
	}
	if temp {
		os.Remove(path)
	}
	fmt.Fprintf(env.Stdout, "Updated plan of %s (%d tasks)\n", id, len(p.Tasks))
	printWarnings(env, p)
	fmt.Fprintf(env.Stdout, "\nNext: `mill plan show %s`, then `mill plan approve %s`.\n", id, id)
	return ExitOK
}

func runPlanApprove(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "plan approve")
	id, code, ok := goalArg(env, "plan approve", "mill plan approve <goal>", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill plan approve: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	g, err := s.GetGoal(ctx, id)
	if err != nil {
		return fail(err)
	}
	if g.Status != store.GoalDraft {
		return fail(fmt.Errorf("goal %s is %s, not draft: only a draft plan can be approved", id, g.Status))
	}
	p, err := loadPlan(g.Plan)
	if err != nil {
		fmt.Fprintf(env.Stderr, "mill plan approve: the stored plan of %s is not valid:\n", id)
		problems(env.Stderr, "", err)
		fmt.Fprintf(env.Stderr, "Fix it with `mill plan edit %s`.\n", id)
		return ExitError
	}
	if err := s.ApproveGoal(ctx, id, p); err != nil {
		return fail(err)
	}
	fmt.Fprintf(env.Stdout, "Approved %s: %d tasks created\n", id, len(p.Tasks))
	fmt.Fprintf(env.Stdout, "\nNext: `mill run %s`.\n", id)
	return ExitOK
}

func runGoal(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		w := env.Stderr
		code := ExitUsage
		if len(args) > 0 {
			w, code = env.Stdout, ExitOK
		}
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  mill goal list [--json]         list all goals, oldest first")
		fmt.Fprintln(w, "  mill goal show <goal> [--json]  show a goal, its tasks and spend")
		return code
	}
	switch args[0] {
	case "list":
		return runGoalList(ctx, env, args[1:])
	case "show":
		return runGoalShow(ctx, env, args[1:])
	}
	fmt.Fprintf(env.Stderr, "mill goal: unknown subcommand %q\n", args[0])
	return ExitUsage
}

type goalSummary struct {
	ID            string       `json:"id"`
	Title         string       `json:"title"`
	Status        string       `json:"status"`
	TasksTotal    int          `json:"tasks_total"`
	TasksVerified int          `json:"tasks_verified"`
	Usage         budget.Usage `json:"usage"`
}

func runGoalList(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "goal list")
	asJSON := fs.Bool("json", false, "print one JSON document")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill goal list: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	goals, err := s.ListGoals(ctx)
	if err != nil {
		return fail(err)
	}
	rows := []goalSummary{}
	for _, g := range goals {
		tasks, err := s.GoalTasks(ctx, g.ID)
		if err != nil {
			return fail(err)
		}
		u, err := s.GoalUsage(ctx, g.ID)
		if err != nil {
			return fail(err)
		}
		r := goalSummary{ID: g.ID, Title: g.Title, Status: g.Status, TasksTotal: len(tasks), Usage: u}
		for _, t := range tasks {
			if store.Done(t.Status) {
				r.TasksVerified++
			}
		}
		rows = append(rows, r)
	}
	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema string        `json:"schema"`
			Goals  []goalSummary `json:"goals"`
		}{"mill/goal-list@1", rows}); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "no goals")
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTASKS\tTOKENS\tCOST\tTITLE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%.2f\t%s\n", r.ID, r.Status, r.TasksVerified, r.TasksTotal,
			fmtInt(r.Usage.Tokens(budget.CountBillable)), r.Usage.CostUSD, r.Title)
	}
	tw.Flush()
	return ExitOK
}

type goalTask struct {
	store.TaskSummary
	LocalID   string   `json:"local_id"`
	DependsOn []string `json:"depends_on"`
}

func runGoalShow(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "goal show")
	asJSON := fs.Bool("json", false, "print one JSON document")
	id, code, ok := goalArg(env, "goal show", "mill goal show <goal> [--json]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill goal show: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	v, err := loadGoalView(ctx, s, id)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema string       `json:"schema"`
			Goal   store.Goal   `json:"goal"`
			Usage  budget.Usage `json:"usage"`
			Tasks  []goalTask   `json:"tasks"`
		}{"mill/goal@1", v.Goal, v.Usage, v.Tasks}); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	printGoalView(env.Stdout, v)
	return ExitOK
}

// goalView is a goal with its spend and tasks, as `mill goal show` prints it.
type goalView struct {
	Goal  store.Goal
	Usage budget.Usage
	Tasks []goalTask
}

func loadGoalView(ctx context.Context, s *store.Store, id string) (goalView, error) {
	var v goalView
	var err error
	if v.Goal, err = s.GetGoal(ctx, id); err != nil {
		return v, err
	}
	tasks, err := s.GoalTasks(ctx, id)
	if err != nil {
		return v, err
	}
	sums, err := s.TaskSummaries(ctx, "")
	if err != nil {
		return v, err
	}
	sumByID := map[string]store.TaskSummary{}
	for _, x := range sums {
		sumByID[x.ID] = x
	}
	if v.Usage, err = s.GoalUsage(ctx, id); err != nil {
		return v, err
	}
	v.Tasks = []goalTask{}
	for _, t := range tasks {
		deps, err := s.TaskDeps(ctx, t.ID)
		if err != nil {
			return v, err
		}
		local := []string{}
		for _, d := range deps {
			_, l := store.SplitTaskID(d)
			local = append(local, l)
		}
		_, l := store.SplitTaskID(t.ID)
		v.Tasks = append(v.Tasks, goalTask{TaskSummary: sumByID[t.ID], LocalID: l, DependsOn: local})
	}
	return v, nil
}

// printGoalView prints a goal's status, spend and task table (`mill goal show`).
func printGoalView(w io.Writer, v goalView) {
	fmt.Fprintf(w, "%s: %s\n", v.Goal.ID, v.Goal.Title)
	fmt.Fprintf(w, "  status    %s\n", v.Goal.Status)
	if v.Goal.Reason != "" {
		fmt.Fprintf(w, "  reason    %s\n", v.Goal.Reason)
	}
	fmt.Fprintf(w, "  spend     %s tokens (billable), $%.2f\n", fmtInt(v.Usage.Tokens(budget.CountBillable)), v.Usage.CostUSD)
	if len(v.Tasks) == 0 {
		fmt.Fprintf(w, "\nno tasks yet (the plan is %s)\n", v.Goal.Status)
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tDEPENDS ON\tATTEMPTS\tTOKENS\tCOST")
	for _, r := range v.Tasks {
		st := r.Status
		if r.Phase != "" {
			st += " (" + r.Phase + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%.2f\n", r.LocalID, st, strings.Join(r.DependsOn, ","),
			r.Attempts, fmtInt(r.Usage.Tokens(budget.CountBillable)), r.Usage.CostUSD)
	}
	tw.Flush()
}

// editTemp writes data to a new temporary file named after prefix, runs $EDITOR (default vi)
// on it and returns the file's path. The file is kept when the editor succeeds, so the caller
// can report it or remove it; when the editor fails the file is removed.
func editTemp(ctx context.Context, env Env, prefix string, data []byte) (string, error) {
	f, err := os.CreateTemp("", prefix+"-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Dir = env.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("editor %q failed: %w", editor, err)
	}
	return path, nil
}
