package cli

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/host"
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
	paths, cfg, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	r, err := tui.NewExecRunner(paths.Root)
	if err != nil {
		return fail(err)
	}
	opts := tuiHostOptions(env, cfg.Host.Kind, cfg.Host.TmuxBin, paths.Root)
	opts.Store, opts.Paths, opts.Runner = s, paths, r
	m := tui.New(opts)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithOutput(env.Stdout))
	if _, err := p.Run(); err != nil {
		return fail(fmt.Errorf("the terminal UI stopped: %w", err))
	}
	return ExitOK
}

// tuiHostOptions returns the tui.Options that depend on the environment and the host config.
// The TUI starts no agents itself, so a host that cannot be selected does not stop it: the
// error goes to HostErr and HostKind stays empty.
func tuiHostOptions(env Env, kind, tmuxBin, root string) tui.Options {
	opts := tui.Options{Getenv: env.getenv, LookPath: env.lookPath, TmuxBin: tmuxBin}
	h, _, err := host.Select(kind, host.Settings{TmuxBin: tmuxBin, RepoRoot: root}, env.getenv, env.lookPath)
	if err != nil {
		opts.HostErr = err
		return opts
	}
	opts.HostKind = h.Name()
	return opts
}
