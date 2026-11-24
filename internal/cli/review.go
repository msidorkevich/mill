package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
)

const reviewUsage = "Usage: mill review [--json] | show <id> [--diff] [--json] | changes <id> (-m <text> | --file <path>) | reject <id> -m <reason> | approve (<id>... | --goal <g>)"

func runReview(ctx context.Context, env Env, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return runReviewShow(ctx, env, args[1:])
		case "changes":
			return runReviewChanges(ctx, env, args[1:])
		case "reject":
			return runReviewReject(ctx, env, args[1:])
		case "approve":
			return runReviewApprove(ctx, env, args[1:])
		}
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(env.Stderr, "mill review: unknown subcommand %q\n%s\n", args[0], reviewUsage)
			return ExitUsage
		}
	}
	return runReviewList(ctx, env, args)
}

func runReviewList(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review")
	asJSON := fs.Bool("json", false, "print one JSON document")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill review: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	tasks, err := s.ReviewTasks(ctx)
	if err != nil {
		return fail(err)
	}
	rows := []reviewRow{}
	for _, t := range tasks {
		r := reviewRow{ReviewTask: t, Status: t.Status}
		if t.Approved && t.Status == store.TaskVerified {
			r.Status = "approved"
			if r.Waiting, err = queuedReason(ctx, s, t.ID); err != nil {
				return fail(err)
			}
		}
		rows = append(rows, r)
	}
	if *asJSON {
		doc := struct {
			Schema string      `json:"schema"`
			Tasks  []reviewRow `json:"tasks"`
		}{"mill/review-list@1", rows}
		return encodeJSON(env, "mill review", doc)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "nothing to review")
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tATTEMPTS\tCOST\tTITLE\tBRANCH\tWAITING FOR")
	for _, t := range rows {
		branch := "-"
		if t.Status == store.TaskPublished && t.PublishedRef != "" {
			branch = t.PublishedRef
		}
		waiting := "-"
		if t.Status == "approved" {
			if t.Waiting != "" {
				waiting = t.Waiting
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.2f\t%s\t%s\t%s\n", t.ID, t.Status, t.Attempts, t.Usage.CostUSD, t.Title, branch, waiting)
	}
	tw.Flush()
	return ExitOK
}

// reviewRow is one task of `mill review`: a verified task with a queued approval has status
// approved and says what it waits for.
type reviewRow struct {
	store.ReviewTask
	Status  string `json:"status"`
	Waiting string `json:"waiting,omitempty"`
}

func encodeJSON(env Env, name string, v any) int {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", name, err)
		return ExitError
	}
	return ExitOK
}

type reviewEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type reviewView struct {
	Schema   string                 `json:"schema"`
	Task     store.Task             `json:"task"`
	Branch   store.Branch           `json:"branch"`
	Attempts int                    `json:"attempts"`
	Usage    budget.Usage           `json:"usage"`
	Verdict  *prompt.VerifierOutput `json:"verdict,omitempty"`
	Gate     *gate.Result           `json:"gate,omitempty"`
	Approval string                 `json:"approval,omitempty"` // "approved <timestamp> at attempt <n>" while an approval stands
	Waiting  string                 `json:"waiting,omitempty"`  // what a queued approval waits for
	Events   []reviewEvent          `json:"events"`             // gate-flaky, owns-outside, gate-base-failing of the last attempt
	From     string                 `json:"from"`               // start of the diff
	DiffStat string                 `json:"diff_stat"`
	Diff     string                 `json:"diff,omitempty"`
}

// reviewable loads a task and refuses one that is not verified or published.
func reviewable(ctx context.Context, s *store.Store, id, verb string) (store.Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return t, err
	}
	if t.Status != store.TaskVerified && t.Status != store.TaskPublished {
		return t, fmt.Errorf("task %s is %s: only a verified or published task can be %s; see `mill task show %s`", id, t.Status, verb, id)
	}
	return t, nil
}

func runReviewShow(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review show")
	asJSON := fs.Bool("json", false, "print one JSON document")
	full := fs.Bool("diff", false, "print the full diff instead of the stat")
	id, code, ok := goalArg(env, "review show", "mill review show <id> [--diff] [--json]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill review show: %v\n", err)
		return ExitError
	}
	paths, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	v := reviewView{Schema: "mill/review@1", Events: []reviewEvent{}}
	if v.Task, err = reviewable(ctx, s, id, "reviewed"); err != nil {
		return fail(err)
	}
	if v.Branch, err = s.GetBranch(ctx, id); err != nil {
		return fail(err)
	}
	if v.Attempts, err = s.MaxAttemptN(ctx, id); err != nil {
		return fail(err)
	}
	if v.Usage, err = s.TaskUsage(ctx, id); err != nil {
		return fail(err)
	}
	if v.Task.Approved {
		v.Approval = fmt.Sprintf("approved %s at attempt %d", v.Task.ApprovedAt, v.Task.ApprovedAttempt)
		if v.Task.Status == store.TaskVerified {
			if v.Waiting, err = queuedReason(ctx, s, id); err != nil {
				return fail(err)
			}
		}
	}
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return fail(err)
	}
	for i := len(evs) - 1; i >= 0 && (v.Verdict == nil || v.Gate == nil); i-- {
		switch evs[i].Kind {
		case "task-verdict":
			if v.Verdict == nil {
				var d struct {
					Verdict prompt.VerifierOutput `json:"verdict"`
				}
				if err := json.Unmarshal(evs[i].Data, &d); err != nil {
					return fail(fmt.Errorf("corrupt task-verdict event %d: %w", evs[i].Seq, err))
				}
				v.Verdict = &d.Verdict
			}
		case "task-gate":
			if v.Gate == nil {
				var d struct {
					Result gate.Result `json:"result"`
				}
				if err := json.Unmarshal(evs[i].Data, &d); err != nil {
					return fail(fmt.Errorf("corrupt task-gate event %d: %w", evs[i].Seq, err))
				}
				v.Gate = &d.Result
			}
		}
	}
	for _, e := range evs {
		switch e.Kind {
		case "gate-flaky", "owns-outside", "gate-base-failing":
			var d struct {
				Attempt int `json:"attempt"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fail(fmt.Errorf("corrupt %s event %d: %w", e.Kind, e.Seq, err))
			}
			if d.Attempt == v.Attempts {
				v.Events = append(v.Events, reviewEvent{e.Kind, e.Data})
			}
		}
	}

	v.From = v.Branch.StartSHA
	if v.From == "" {
		v.From = v.Branch.BaseSHA
	}
	if v.From == "" || v.Branch.HeadSHA == "" {
		return fail(fmt.Errorf("branch %s has no recorded start or head commit, so there is nothing to diff; run `mill task show %s`", v.Branch.Name, id))
	}
	rng := v.From + ".." + v.Branch.HeadSHA
	if v.DiffStat, err = gitx.Run(ctx, paths.Root, "diff", "--no-color", "--no-ext-diff", "--stat", rng); err != nil {
		return fail(fmt.Errorf("diff stat %s: %w", rng, err))
	}
	if *full {
		if v.Diff, err = gitx.Run(ctx, paths.Root, "diff", "--no-color", "--no-ext-diff", rng); err != nil {
			return fail(fmt.Errorf("diff %s: %w", rng, err))
		}
	}
	if *asJSON {
		return encodeJSON(env, "mill review show", v)
	}
	printReview(env, v)
	return ExitOK
}

func printReview(env Env, v reviewView) {
	w := env.Stdout
	t := v.Task
	fmt.Fprintf(w, "%s: %s\n", t.ID, t.Title)
	fmt.Fprintf(w, "  status    %s\n", t.Status)
	fmt.Fprintf(w, "  attempts  %d\n", v.Attempts)
	fmt.Fprintf(w, "  cost      $%.2f\n", v.Usage.CostUSD)
	fmt.Fprintf(w, "  branch    %s (start %s, head %s)\n", v.Branch.Name, short(v.From), short(v.Branch.HeadSHA))
	if v.Branch.PublishedRef != "" {
		fmt.Fprintf(w, "  published %s\n", v.Branch.PublishedRef)
	}
	if v.Approval != "" {
		fmt.Fprintf(w, "  approval  %s\n", v.Approval)
		if v.Waiting != "" {
			fmt.Fprintf(w, "  waiting   will publish when %s\n", v.Waiting)
		}
	}
	fmt.Fprintf(w, "\nSpec:\n  %s\n", strings.ReplaceAll(t.Spec.Spec, "\n", "\n  "))
	fmt.Fprintln(w, "\nAcceptance:")
	for _, a := range t.Acceptance {
		fmt.Fprintf(w, "  - %s\n", a)
	}
	if v.Verdict != nil {
		fmt.Fprintf(w, "\nLast verdict: %s: %s\n", v.Verdict.Verdict, v.Verdict.Summary)
		for _, f := range v.Verdict.Findings {
			fmt.Fprintf(w, "  - %s\n", strings.ReplaceAll(f.String(), "\n", "\n    "))
		}
	} else {
		fmt.Fprintln(w, "\nLast verdict: none recorded")
	}
	if v.Gate != nil {
		res := "pass"
		if !v.Gate.Passed {
			res = "fail"
		}
		fmt.Fprintf(w, "\nLast gate: %s\n", res)
		for _, c := range v.Gate.Commands {
			cr := "pass"
			if !c.Passed() {
				cr = "fail"
			}
			fmt.Fprintf(w, "  %s  %s\n", cr, c.Cmd)
		}
	} else {
		fmt.Fprintln(w, "\nLast gate: none recorded")
	}
	if len(v.Events) > 0 {
		fmt.Fprintf(w, "\nNotes from attempt %d:\n", v.Attempts)
		for _, e := range v.Events {
			fmt.Fprintf(w, "  %s: %s\n", e.Kind, string(e.Data))
		}
	}
	if v.Diff != "" {
		fmt.Fprintf(w, "\nDiff %s..%s:\n%s", short(v.From), short(v.Branch.HeadSHA), v.Diff)
		if !strings.HasSuffix(v.Diff, "\n") {
			fmt.Fprintln(w)
		}
		return
	}
	fmt.Fprintf(w, "\nDiff stat %s..%s:\n%s\n", short(v.From), short(v.Branch.HeadSHA), v.DiffStat)
}

var blankLines = regexp.MustCompile(`\n[ \t]*\n`)

// paragraphs splits text on blank lines into trimmed, non-empty paragraphs.
func paragraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	for _, p := range blankLines.Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runReviewChanges(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review changes")
	msg := fs.String("m", "", "the operator's comments; blank lines separate findings")
	file := fs.String("file", "", "read the comments from this file")
	id, code, ok := goalArg(env, "review changes", "mill review changes <id> (-m <text> | --file <path>)", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill review changes: %v\n", err)
		return ExitError
	}
	if (*msg == "") == (*file == "") {
		fmt.Fprintln(env.Stderr, "mill review changes: give the comments with exactly one of -m <text> or --file <path>")
		return ExitUsage
	}
	text := *msg
	if *file != "" {
		b, err := os.ReadFile(absPath(env, *file))
		if err != nil {
			return fail(err)
		}
		text = string(b)
	}
	paras := paragraphs(text)
	if len(paras) == 0 {
		return fail(errors.New("the comments are empty; write at least one comment"))
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	release, err := claimTasks(ctx, env, s, "review changes", id)
	if err != nil {
		return fail(err)
	}
	defer release()
	t, err := reviewable(ctx, s, id, "sent back for changes")
	if err != nil {
		return fail(err)
	}
	n, err := s.MaxAttemptN(ctx, id)
	if err != nil {
		return fail(err)
	}
	findings := make([]prompt.Finding, 0, len(paras))
	for _, p := range paras {
		findings = append(findings, prompt.Finding{Source: "operator", Issue: p})
	}
	if err := s.RequestChanges(ctx, id, n, findings, "changes requested"); err != nil {
		return fail(err)
	}
	fmt.Fprintf(env.Stdout, "Recorded %d finding(s) for %s; it is pending again.\n", len(findings), id)
	if err := reopenGoal(ctx, env, s, id, "changes requested", "mill run %s"); err != nil {
		return fail(err)
	}
	if t.Status == store.TaskPublished {
		fmt.Fprintf(env.Stdout, "Note: %s stays published on its remote branch until it is republished.\n", id)
	}
	if goalID, _ := store.SplitTaskID(id); goalID != "" {
		fmt.Fprintf(env.Stdout, "\nNext: `mill run %s`.\n", goalID)
	} else {
		fmt.Fprintln(env.Stdout, "\nNext: `mill task run --file <file> --retry`.")
	}
	return ExitOK
}

// reopenGoal sets the goal of a task that was sent back to approved when the goal is done or
// landed, so `mill run <goal>` accepts it again. Tasks without a goal are left alone.
func reopenGoal(ctx context.Context, env Env, s *store.Store, taskID, why, nextFmt string) error {
	goalID, _ := store.SplitTaskID(taskID)
	if goalID == "" {
		return nil
	}
	g, err := s.GetGoal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("%s was sent back but its goal cannot be loaded: %w; run `mill goal show %s`", taskID, err, goalID)
	}
	if g.Status != store.GoalDone && g.Status != store.GoalLanded {
		return nil
	}
	reason := fmt.Sprintf("reopened because %s was sent back (%s)", taskID, why)
	if err := s.SetGoalStatus(ctx, goalID, store.GoalApproved, reason); err != nil {
		return fmt.Errorf("%s was sent back but goal %s could not be reopened: %w; run `mill plan approve %s`", taskID, goalID, err, goalID)
	}
	fmt.Fprintf(env.Stdout, "Goal %s was %s and is reopened (approved). Next: `%s`.\n", goalID, g.Status, fmt.Sprintf(nextFmt, goalID))
	return nil
}

func runReviewReject(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review reject")
	msg := fs.String("m", "", "why the task is rejected")
	id, code, ok := goalArg(env, "review reject", "mill review reject <id> -m <reason>", fs, args)
	if !ok {
		return code
	}
	if strings.TrimSpace(*msg) == "" {
		fmt.Fprintln(env.Stderr, "mill review reject: give the reason with -m <reason>")
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill review reject: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	release, err := claimTasks(ctx, env, s, "review reject", id)
	if err != nil {
		return fail(err)
	}
	defer release()
	if _, err := reviewable(ctx, s, id, "rejected"); err != nil {
		return fail(err)
	}
	held, err := s.RejectTask(ctx, id, "rejected: "+strings.TrimSpace(*msg))
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(env.Stdout, "Rejected %s: blocked with reason %q.\n", id, "rejected: "+strings.TrimSpace(*msg))
	if err := reopenGoal(ctx, env, s, id, "rejected", "mill run %s --retry-blocked"); err != nil {
		return fail(err)
	}
	if len(held) == 0 {
		fmt.Fprintln(env.Stdout, "No other task depends on it.")
		return ExitOK
	}
	fmt.Fprintln(env.Stdout, "Put on hold:")
	for _, h := range held {
		fmt.Fprintf(env.Stdout, "  %s\n", h)
	}
	return ExitOK
}

const approveUsage = "mill review approve (<id>... | --goal <g>)"

func runReviewApprove(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review approve")
	goalID := fs.String("goal", "", "approve every verified or published task of this goal, dependencies first")
	var ids []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ids, args = append(ids, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flagErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	ids = append(ids, fs.Args()...)
	if (*goalID == "") == (len(ids) == 0) {
		fmt.Fprintln(env.Stderr, "mill review approve: give one or more task ids or --goal <g>, not both\nUsage: "+approveUsage)
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill review approve: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	if *goalID != "" {
		if ids, err = goalReviewOrder(ctx, s, *goalID); err != nil {
			return fail(err)
		}
		if len(ids) == 0 {
			return fail(fmt.Errorf("goal %s has no verified or published task to approve; see `mill review`", *goalID))
		}
	}
	code := ExitOK
	for _, id := range ids {
		if err := approveClaimed(ctx, env, s, publish.Options{Store: s, Cfg: cfg, Paths: paths, TaskID: id}); err != nil {
			fmt.Fprintf(env.Stderr, "mill review approve: %v\n", err)
			code = ExitError
		}
	}
	return code
}

// approveClaimed claims one task for the whole of its publish (the store updates before
// and after the push and the push itself) and releases the claim when it is done. A busy
// task is refused with a *store.BusyError and left as it is.
func approveClaimed(ctx context.Context, env Env, s *store.Store, o publish.Options) error {
	release, err := claimTasks(ctx, env, s, "review approve", o.TaskID)
	if err != nil {
		return err
	}
	defer release()
	return approveOne(ctx, env, s, o)
}

// claimTasks claims ids for this process running the mill subcommand command, so no other
// mill command changes them meanwhile. The returned release deletes the claims and reports
// a failure to do so on stderr; call it on every exit path.
func claimTasks(ctx context.Context, env Env, s *store.Store, command string, ids ...string) (release func(), err error) {
	c := store.NewClaimer(command)
	if err := s.ClaimTasks(ctx, c, ids); err != nil {
		return nil, err
	}
	return func() {
		// Release even when ctx was cancelled, or the claim would outlive the command.
		if err := s.ReleaseClaims(context.WithoutCancel(ctx), c, ids); err != nil {
			fmt.Fprintf(env.Stderr, "mill %s: %v; the claim is dropped once this process exits\n", command, err)
		}
	}, nil
}

// approveOne publishes one task, or queues its approval when only an unpublished dependency
// stands in the way.
func approveOne(ctx context.Context, env Env, s *store.Store, o publish.Options) error {
	id := o.TaskID
	res, err := publish.Publish(ctx, o)
	if res.GateRerun != nil && err == nil {
		fmt.Fprintf(env.Stdout, "Gate re-run at the head: passed (%d command(s)).\n", len(res.GateRerun.Commands))
	}
	if err != nil {
		if _, ok := publish.ParentRefusal(err); !ok {
			return err
		}
		t, terr := s.GetTask(ctx, id)
		if terr != nil {
			return fmt.Errorf("%v; and the task cannot be loaded to queue its approval: %w", err, terr)
		}
		if t.Status != store.TaskVerified {
			return err
		}
		n, nerr := s.MaxAttemptN(ctx, id)
		if nerr != nil {
			return fmt.Errorf("%v; and its latest attempt cannot be read to queue its approval: %w", err, nerr)
		}
		reason := waitingReason(err)
		if err := s.ApproveTask(ctx, id, n); err != nil {
			return err
		}
		goalID, _ := store.SplitTaskID(id)
		if _, err := s.AppendEvent(ctx, approvalWaitingEvent, goalID, id, approvalWaiting{Attempt: n, Reason: reason}); err != nil {
			return fmt.Errorf("approved %s but could not record why it waits: %w; approve it again", id, err)
		}
		fmt.Fprintf(env.Stdout, "approved %s: queued, will publish when %s\n", id, reason)
		return nil
	}
	verb := "Published"
	if res.Republish {
		verb = "Republished"
	}
	fmt.Fprintf(env.Stdout, "%s %s as %s on %s at %s (parent %s).\n", verb, id, res.Ref, res.Remote, short(res.SHA), short(res.Parent))
	fmt.Fprintf(env.Stdout, "\nNext: open a pull request from %s into %s.\n", res.Ref, res.Onto)
	return nil
}

// approvalWaitingEvent records why a queued approval cannot publish yet; it follows the
// task-approved event of the same approval.
const approvalWaitingEvent = "task-approval-waiting"

type approvalWaiting struct {
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
}

// waitingReason turns a dependency refusal into what the queued approval waits for.
func waitingReason(err error) string {
	var re *publish.RefusedError
	if !errors.As(err, &re) {
		return "its dependencies are published or landed"
	}
	switch {
	case strings.Contains(re.Msg, "every other dependency must have landed"):
		return "the other dependencies land"
	case strings.Contains(re.Msg, "its branch is still based on"):
		return "`mill sync` moves it onto the base branch"
	case strings.Contains(re.Msg, "does not hold the parent's head") && re.Parent != "":
		return fmt.Sprintf("`mill sync` or `mill keep` restacks it onto the new head of %s", re.Parent)
	case strings.Contains(re.Msg, "that are not published") && re.Parent != "":
		return fmt.Sprintf("parent %s publishes its new changes", re.Parent)
	case re.Parent != "":
		return fmt.Sprintf("parent %s is published or landed", re.Parent)
	}
	return "its dependencies are published or landed"
}

// queuedReason returns the reason recorded with the task's current approval, "" if none.
func queuedReason(ctx context.Context, s *store.Store, id string) (string, error) {
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return "", err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		switch evs[i].Kind {
		case "task-approved":
			return "", nil
		case approvalWaitingEvent:
			var d approvalWaiting
			if err := json.Unmarshal(evs[i].Data, &d); err != nil {
				return "", fmt.Errorf("corrupt %s event %d: %w", approvalWaitingEvent, evs[i].Seq, err)
			}
			return d.Reason, nil
		}
	}
	return "", nil
}

// goalReviewOrder lists a goal's verified and published tasks, dependencies before
// dependents (plan order otherwise).
func goalReviewOrder(ctx context.Context, s *store.Store, goalID string) ([]string, error) {
	if _, err := s.GetGoal(ctx, goalID); err != nil {
		return nil, err
	}
	tasks, err := s.GoalTasks(ctx, goalID)
	if err != nil {
		return nil, err
	}
	deps := map[string][]string{}
	for _, t := range tasks {
		if deps[t.ID], err = s.TaskDeps(ctx, t.ID); err != nil {
			return nil, err
		}
	}
	var out []string
	done := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if done[id] {
			return
		}
		done[id] = true
		for _, d := range deps[id] {
			visit(d)
		}
		out = append(out, id)
	}
	for _, t := range tasks {
		visit(t.ID)
	}
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	ids := out[:0]
	for _, id := range out {
		if st := status[id]; st == store.TaskVerified || st == store.TaskPublished {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
