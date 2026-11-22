package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/msidorkevich/mill/internal/syncer"
)

func runSync(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "sync")
	asJSON := fs.Bool("json", false, "print one JSON document")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill sync: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	// With --json, stdout holds only the document; progress goes nowhere.
	var out io.Writer = env.Stdout
	recoverEnv := env
	if *asJSON {
		out = nil
		recoverEnv.Stdout = env.Stderr
	}
	release, err := lockAndRecover(ctx, recoverEnv, "sync", paths, s, cfg.Prices)
	if err != nil {
		return fail(err)
	}
	defer release()

	res, err := syncer.Sync(ctx, syncer.Options{Store: s, Cfg: cfg, Paths: paths, Out: out})
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		return encodeJSON(env, "mill sync", struct {
			Schema string `json:"schema"`
			syncer.Result
		}{"mill/sync@1", res})
	}
	printSync(env.Stdout, res)
	return ExitOK
}

func printSync(w io.Writer, r syncer.Result) {
	fmt.Fprintf(w, "\nSynced with %s/%s at %s.\n", r.Remote, r.BaseBranch, short(r.BaseSHA))
	if len(r.Landed)+len(r.Restacked)+len(r.Republished)+len(r.NeedsRepublish)+len(r.Blocked)+len(r.GoalsLanded) == 0 {
		fmt.Fprintln(w, "Nothing changed.")
		return
	}
	if len(r.Landed) > 0 {
		fmt.Fprintln(w, "Landed:")
		for _, l := range r.Landed {
			fmt.Fprintf(w, "  %s  %s in %s\n", l.TaskID, l.Method, short(l.Commit))
		}
	}
	if len(r.Restacked) > 0 {
		fmt.Fprintln(w, "Restacked:")
		for _, x := range r.Restacked {
			fmt.Fprintf(w, "  %s  onto %s (%s)\n", x.TaskID, x.Onto, short(x.OntoSHA))
		}
	}
	if len(r.Republished) > 0 {
		fmt.Fprintln(w, "Republished:")
		for _, p := range r.Republished {
			fmt.Fprintf(w, "  %s  %s at %s\n", p.TaskID, p.Ref, short(p.SHA))
		}
	}
	if len(r.NeedsRepublish) > 0 {
		fmt.Fprintln(w, "Needs re-publishing after its parent (restacked; the review branch still shows the old commit):")
		for _, n := range r.NeedsRepublish {
			after := n.After
			if after == "" {
				after = "<parent>"
			}
			fmt.Fprintf(w, "  %s  after %s: run `mill review approve %s`, then `mill sync`\n", n.TaskID, after, after)
		}
	}
	if len(r.Blocked) > 0 {
		fmt.Fprintln(w, "Blocked:")
		for _, b := range r.Blocked {
			fmt.Fprintf(w, "  %s  %s\n", b.TaskID, strings.ReplaceAll(b.Reason, "\n", "\n    "))
		}
	}
	if len(r.GoalsLanded) > 0 {
		fmt.Fprintf(w, "Goals landed: %s\n", strings.Join(r.GoalsLanded, ", "))
	}
	if len(r.Blocked) > 0 {
		fmt.Fprintln(w, "\nNext: inspect blocked tasks with `mill task show <id>`.")
	}
}
