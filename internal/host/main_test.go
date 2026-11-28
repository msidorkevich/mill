package host

import (
	"os"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// TestMain hides the operator's herdr and tmux sessions, so no test selects the herdr host
// from the inherited environment and reaches a real herdr, and no tmux host opens windows
// in the operator's tmux session. Tests that need the herdr host set HERDR_ENV themselves
// with a fake herdr first on PATH; tmux tests pass their environment through Tmux.Getenv.
// It also keeps every git the tests start from running background maintenance.
func TestMain(m *testing.M) {
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
