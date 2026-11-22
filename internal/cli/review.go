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

	"mill/internal/budget"
	"mill/internal/gate"
	"mill/internal/gitx"
	"mill/internal/prompt"
	"mill/internal/publish"
	"mill/internal/runlock"
	"mill/internal/store"
)

const reviewUsage = "Usage: mill review [--json] | show <id> [--diff] [--json] | changes <id> (-m <text> | --file <path>) | reject <id> -m <reason> | approve <id>"

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
	if tasks == nil {
		tasks = []store.ReviewTask{}
	}
	if *asJSON {
		doc := struct {
			Schema string             `json:"schema"`
			Tasks  []store.ReviewTask `json:"tasks"`
		}{"mill/review-list@1", tasks}
		return encodeJSON(env, "mill review", doc)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(env.Stdout, "nothing to review")
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tATTEMPTS\tCOST\tTITLE\tBRANCH")
	for _, t := range tasks {
		branch := "-"
		if t.Status == store.TaskPublished && t.PublishedRef != "" {
			branch = t.PublishedRef
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.2f\t%s\t%s\n", t.ID, t.Status, t.Attempts, t.Usage.CostUSD, t.Title, branch)
	}
	tw.Flush()
	return ExitOK
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
	Events   []reviewEvent          `json:"events"` // gate-flaky, owns-outside, gate-base-failing of the last attempt
	From     string                 `json:"from"`   // start of the diff
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
	if _, err := reviewable(ctx, s, id, "rejected"); err != nil {
		return fail(err)
	}
	held, err := s.RejectTask(ctx, id, "rejected: "+strings.TrimSpace(*msg))
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(env.Stdout, "Rejected %s: blocked with reason %q.\n", id, "rejected: "+strings.TrimSpace(*msg))
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

func runReviewApprove(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "review approve")
	id, code, ok := goalArg(env, "review approve", "mill review approve <id>", fs, args)
	if !ok {
		return code
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
	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "mill review approve: %v\n", err)
		}
	}()

	res, err := publish.Publish(ctx, publish.Options{Store: s, Cfg: cfg, Paths: paths, TaskID: id})
	if res.GateRerun != nil && err == nil {
		fmt.Fprintf(env.Stdout, "Gate re-run at the head: passed (%d command(s)).\n", len(res.GateRerun.Commands))
	}
	if err != nil {
		return fail(err)
	}
	verb := "Published"
	if res.Republish {
		verb = "Republished"
	}
	fmt.Fprintf(env.Stdout, "%s %s as %s on %s at %s (parent %s).\n", verb, id, res.Ref, res.Remote, short(res.SHA), short(res.Parent))
	fmt.Fprintf(env.Stdout, "\nNext: open a pull request from %s into %s.\n", res.Ref, res.Onto)
	return ExitOK
}
