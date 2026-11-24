package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/msidorkevich/mill/internal/store"
)

func runStats(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		w := env.Stderr
		code := ExitUsage
		if len(args) > 0 {
			w, code = env.Stdout, ExitOK
		}
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  mill stats models [--json]  first-pass rate, attempts and cost by size, complexity and model")
		return code
	}
	switch args[0] {
	case "models":
		return runStatsModels(ctx, env, args[1:])
	}
	fmt.Fprintf(env.Stderr, "mill stats: unknown subcommand %q\n", args[0])
	return ExitUsage
}

type statsModelsDoc struct {
	Schema string            `json:"schema"`
	Models []store.ModelStat `json:"models"`
}

func runStatsModels(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "stats models")
	asJSON := fs.Bool("json", false, "print one JSON document")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill stats models: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	stats, err := s.ModelStats(ctx)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		return encodeJSON(env, "mill stats models", statsModelsDoc{Schema: "mill/stats-models@1", Models: stats})
	}
	if len(stats) == 0 {
		fmt.Fprintln(env.Stdout, "no tasks with attempts yet")
		return ExitOK
	}
	printModelStats(env.Stdout, stats)
	return ExitOK
}

func printModelStats(w io.Writer, stats []store.ModelStat) {
	fmt.Fprintf(w, "%-5s %-10s %-20s %6s %8s %12s %9s\n", "SIZE", "COMPLEXITY", "MODEL", "TASKS", "1ST-PASS", "AVG ATTEMPTS", "AVG $")
	for _, m := range stats {
		fmt.Fprintf(w, "%-5s %-10s %-20s %6d %7.0f%% %12.2f %9.2f\n",
			m.Size, m.Complexity, m.Model, m.Tasks, m.FirstAttemptPass*100, m.AvgAttempts, m.AvgCostUSD)
	}
}
