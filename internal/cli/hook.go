package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/store"
)

const hookUsage = "Usage: mill hook run <task> [--event publish|update|status]"

func runHook(ctx context.Context, env Env, args []string) int {
	if len(args) > 0 && args[0] == "run" {
		return runHookRun(ctx, env, args[1:])
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(env.Stdout, hookUsage)
		return ExitOK
	}
	if len(args) == 0 {
		fmt.Fprintf(env.Stderr, "mill hook: give a subcommand\n%s\n", hookUsage)
	} else {
		fmt.Fprintf(env.Stderr, "mill hook: unknown subcommand %q\n%s\n", args[0], hookUsage)
	}
	return ExitUsage
}

// runHookRun re-runs a published task's on_publish, on_update or status hook with a fresh
// document. A status run prints the parsed status and records it as a ci-status event when it
// changed.
func runHookRun(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "hook run")
	event := fs.String("event", "", "the hook to run: publish, update or status (default: publish until a publish hook succeeded, then update)")
	id, code, ok := goalArg(env, "hook run", "mill hook run <task> [--event publish|update|status]", fs, args)
	if !ok {
		return code
	}
	if *event != "" && *event != hook.EventPublish && *event != hook.EventUpdate && *event != hook.EventStatus {
		fmt.Fprintf(env.Stderr, "mill hook run: unknown --event %q: use publish or update to re-run that hook, or status to read the CI status\n%s\n", *event, hookUsage)
		return ExitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill hook run: %v\n", err)
		return ExitError
	}
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	t, err := s.GetTask(ctx, id)
	if err != nil {
		return fail(err)
	}
	if t.Status != store.TaskPublished {
		return fail(fmt.Errorf("task %s is %s, not published: a hook runs only for a published task; publish it first with `mill review approve %s`",
			id, t.Status, id))
	}
	if *event == "" {
		if *event, err = defaultHookEvent(ctx, s, id); err != nil {
			return fail(err)
		}
	}
	command := cfg.OnPublish
	switch *event {
	case hook.EventUpdate:
		command = cfg.OnUpdate
	case hook.EventStatus:
		command = cfg.Status
	}
	if strings.TrimSpace(command) == "" {
		if *event == hook.EventStatus {
			return fail(fmt.Errorf("hook status is not configured; set status in .mill/config.toml"))
		}
		return fail(fmt.Errorf("hook on_%s is not configured; set it in .mill/config.toml", *event))
	}

	doc, err := hook.Build(ctx, s, cfg, paths.Root, id, *event, "")
	if err != nil {
		return fail(err)
	}
	r := hook.Runner{Store: s, Cfg: cfg, Root: paths.Root, Stderr: env.Stderr}
	res, err := r.Run(ctx, *event, t.GoalID, id, doc)
	if err != nil {
		return fail(err)
	}
	if res.Skipped {
		return fail(fmt.Errorf("hook on_%s is not configured; set it in .mill/config.toml", *event))
	}
	if res.Failed() {
		return ExitError // the runner reported the failure on stderr
	}
	if *event == hook.EventStatus {
		st := *res.Status
		if st.Checks == nil {
			st.Checks = []hook.StatusCheck{}
		}
		if _, err := cistatus.RecordIfChanged(ctx, s, id, doc.HeadSHA, st); err != nil {
			return fail(err)
		}
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return fail(fmt.Errorf("encode the status of %s: %w", id, err))
		}
		fmt.Fprintln(env.Stdout, string(b))
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "hook on_%s for %s succeeded in %s\n", *event, id, res.Duration.Round(time.Millisecond))
	if res.Tail != "" {
		fmt.Fprintln(env.Stdout, res.Tail)
	}
	return ExitOK
}

// defaultHookEvent is publish until a publish hook of the task has succeeded, then update.
func defaultHookEvent(ctx context.Context, s *store.Store, id string) (string, error) {
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read the events of %s to choose the hook event; pass --event publish|update: %w", id, err)
	}
	for _, e := range evs {
		if e.Kind != hook.RunEvent {
			continue
		}
		var d struct {
			Event    string `json:"event"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return "", fmt.Errorf("event %d of %s is not a readable %s event; pass --event publish|update: %w", e.Seq, id, hook.RunEvent, err)
		}
		if d.ExitCode == nil {
			return "", fmt.Errorf("event %d of %s has no exit code; pass --event publish|update", e.Seq, id)
		}
		if d.Event == hook.EventPublish && *d.ExitCode == 0 {
			return hook.EventUpdate, nil
		}
	}
	return hook.EventPublish, nil
}
