package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/keeper"
)

const (
	keepUsage       = "Usage: mill keep [<goal>] [--watch] [--interval 5m] [--combined] [--json]"
	keepMinInterval = 10 * time.Second
)

// keepWait blocks for d or until ctx is done; it reports whether the wait completed.
// Tests replace it so they never sleep.
var keepWait = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type keepOpts struct {
	goal     string
	combined bool
	asJSON   bool
}

// keepDoc is the --json document of one pass. Keeper is null when only the combined check ran.
type keepDoc struct {
	Schema   string                 `json:"schema"`
	Keeper   *keeper.Result         `json:"keeper"`
	Combined *keeper.CombinedResult `json:"combined"`
}

func runKeep(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "keep")
	watch := fs.Bool("watch", false, "repeat every --interval until interrupted")
	interval := fs.Duration("interval", 5*time.Minute, "time between passes with --watch (at least 10s)")
	combined := fs.Bool("combined", false, "only fetch and run the combined check (no rebases)")
	asJSON := fs.Bool("json", false, "print one JSON document per pass")
	var goal string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		goal, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flagErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 1 || (fs.NArg() == 1 && goal != "") {
		fmt.Fprintf(env.Stderr, "mill keep: unexpected arguments: %s\n%s\n", strings.Join(fs.Args(), " "), keepUsage)
		return ExitUsage
	}
	if fs.NArg() == 1 {
		goal = fs.Arg(0)
	}
	if *interval < keepMinInterval {
		fmt.Fprintf(env.Stderr, "mill keep: --interval %s is too short; use at least %s\n%s\n", *interval, keepMinInterval, keepUsage)
		return ExitUsage
	}
	o := keepOpts{goal: goal, combined: *combined, asJSON: *asJSON}
	for {
		if code := keepOnce(ctx, env, o); code != ExitOK {
			if *watch && ctx.Err() != nil {
				return ExitOK
			}
			return code
		}
		if !*watch {
			return ExitOK
		}
		if !keepWait(ctx, *interval) || ctx.Err() != nil {
			return ExitOK
		}
	}
}

// keepOnce runs one pass under the run lock and prints its result.
func keepOnce(ctx context.Context, env Env, o keepOpts) int {
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill keep: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	if o.goal != "" {
		if _, err := s.GetGoal(ctx, o.goal); err != nil {
			goals, lerr := s.ListGoals(ctx)
			if lerr != nil {
				return fail(lerr)
			}
			ids := make([]string, 0, len(goals))
			for _, g := range goals {
				ids = append(ids, g.ID)
			}
			list := "none yet (create one with `mill plan`)"
			if len(ids) > 0 {
				list = strings.Join(ids, ", ")
			}
			return fail(fmt.Errorf("unknown goal %q; existing goals: %s", o.goal, list))
		}
	}

	// With --json, stdout holds only the document; progress goes to stderr.
	var progress io.Writer = env.Stdout
	recoverEnv := env
	if o.asJSON {
		progress = env.Stderr
		recoverEnv.Stdout = env.Stderr
	}
	release, err := lockAndRecover(ctx, recoverEnv, "keep", paths, s, cfg.Prices)
	if err != nil {
		return fail(err)
	}
	defer release()

	out := &lockedWriter{w: progress}
	p, err := newPipeline(env, "keep", paths, cfg, "", s, out)
	if err != nil {
		return fail(err)
	}
	ko := keeper.Options{Store: s, Cfg: cfg, Paths: paths, Host: p.Host, Pipeline: p, Goal: o.goal, Out: out, Force: o.combined}

	var kres *keeper.Result
	if o.combined {
		if err := gitx.Fetch(ctx, paths.Root, cfg.Remote); err != nil {
			return fail(fmt.Errorf("fetch %s: %w", cfg.Remote, err))
		}
	} else {
		r, err := keeper.Pass(ctx, ko)
		if err != nil {
			return fail(err)
		}
		kres = &r
	}
	// Combined returns without running when fewer than two branches are ready.
	cres, err := keeper.Combined(ctx, ko)
	if err != nil {
		return fail(err)
	}
	if o.asJSON {
		return encodeJSON(env, "mill keep", keepDoc{Schema: "mill/keep@1", Keeper: kres, Combined: &cres})
	}
	printKeep(env.Stdout, kres, cres)
	return ExitOK
}

func printKeep(w io.Writer, r *keeper.Result, c keeper.CombinedResult) {
	fmt.Fprintln(w)
	empty := true
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		empty = false
		fmt.Fprintf(w, "%s:\n", title)
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	var blocked bool
	if r != nil {
		var landed, rebased, republished, blockedL, queued, waiting []string
		for _, l := range r.Sync.Landed {
			landed = append(landed, fmt.Sprintf("%s  %s in %s", l.TaskID, l.Method, short(l.Commit)))
		}
		for _, x := range r.Rebased {
			rebased = append(rebased, fmt.Sprintf("%s  onto %s (%s)", x.TaskID, x.Onto, short(x.OntoSHA)))
		}
		for _, p := range r.Republished {
			republished = append(republished, fmt.Sprintf("%s  %s at %s", p.TaskID, p.Ref, short(p.SHA)))
		}
		for _, b := range r.Blocked {
			blockedL = append(blockedL, fmt.Sprintf("%s  %s", b.TaskID, strings.ReplaceAll(b.Reason, "\n", "\n    ")))
		}
		for _, p := range r.Queue.Published {
			queued = append(queued, fmt.Sprintf("%s  %s at %s", p.TaskID, p.Ref, short(p.SHA)))
		}
		for _, q := range r.Queue.Waiting {
			waiting = append(waiting, fmt.Sprintf("%s  %s", q.TaskID, strings.ReplaceAll(q.Reason, "\n", "\n    ")))
		}
		blocked = len(r.Blocked) > 0
		section("Landed", landed)
		section("Rebased", rebased)
		section("Resolved by rerere", r.ResolvedRerere)
		section("Resolved by agent", r.ResolvedAgent)
		section("Fixed", r.Fixed)
		section("Republished", republished)
		section("Published from queue", queued)
		section("Waiting", waiting)
		section("Blocked", blockedL)
	}
	cachedNote := ""
	if c.Cached {
		cachedNote = " (cached: nothing changed since the last check)"
	}
	switch {
	case c.Ran && c.Passed:
		empty = false
		fmt.Fprintf(w, "Combined check: passed (%d branches merged: %s)%s.\n", len(c.Branches), strings.Join(c.Branches, ", "), cachedNote)
	case c.Ran && c.Culprit != "":
		empty = false
		fmt.Fprintf(w, "Combined check: failed%s.\n  culprit: %s\n", cachedNote, c.Culprit)
		if c.Partner != "" {
			fmt.Fprintf(w, "  partner: %s\n", c.Partner)
		} else {
			fmt.Fprintln(w, "  partner: none (red with the base alone)")
		}
		if c.Failed != "" {
			fmt.Fprintf(w, "  failed: %s\n", strings.ReplaceAll(c.Failed, "\n", "\n    "))
		}
	case c.Ran:
		empty = false
		fmt.Fprintf(w, "Combined check: failed%s.\n", cachedNote)
	}
	if empty {
		fmt.Fprintln(w, "Nothing to do.")
		return
	}
	if !c.Ran {
		fmt.Fprintln(w, "Combined check: not run (fewer than two branches are ready).")
	}
	if blocked {
		fmt.Fprintln(w, "\nNext: inspect blocked tasks with `mill task show <id>`.")
	}
}
