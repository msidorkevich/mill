package tui

import (
	"os"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// TestMain hides the operator's herdr session, so no test selects the herdr host from the
// inherited environment and reaches a real herdr. Tests that need the herdr host set
// HERDR_ENV themselves with a fake herdr first on PATH. It also keeps every git the tests
// start from running background maintenance.
func TestMain(m *testing.M) {
	for _, k := range []string{"HERDR_ENV", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID"} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	if err := testenv.DisableGitMaintenance(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
