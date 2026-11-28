package cli

import (
	"context"
	"os"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// TestMain hides the operator's herdr and tmux sessions, so no test selects the herdr or
// tmux host from the inherited environment and reaches a real herdr or tmux. Tests that
// need those hosts set HERDR_ENV or TMUX themselves with a fake binary first on PATH. It also keeps every git the tests
// start from running background maintenance.
//
// Started as `<test binary> __pgrun -- <command>`, the binary acts as mill's pgrun helper
// instead of running the tests: the herdr host's default pgrun prefix is the running binary
// with `__pgrun --`, which under `go test` is this test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == pgrunCommand {
		os.Exit(Run(context.Background(), Env{Stdout: os.Stdout, Stderr: os.Stderr}, os.Args[1:]))
	}
	for _, k := range []string{"HERDR_ENV", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "TMUX", "TMUX_PANE"} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	if err := testenv.DisableGitMaintenance(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
