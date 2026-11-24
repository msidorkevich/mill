package cli

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/tui"
)

// runTUI opens the interactive terminal UI over the project's store.
func runTUI(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "tui")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill tui: %v\n", err)
		return ExitError
	}
	paths, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	r, err := tui.NewExecRunner(paths.Root)
	if err != nil {
		return fail(err)
	}
	m := tui.New(tui.Options{Store: s, Paths: paths, Runner: r, Getenv: env.getenv, LookPath: env.lookPath})
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithOutput(env.Stdout))
	if _, err := p.Run(); err != nil {
		return fail(fmt.Errorf("the terminal UI stopped: %w", err))
	}
	return ExitOK
}
