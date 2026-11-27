package cli

import (
	"context"
	"os"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// TestMain hides the operator's herdr session, so no test selects the herdr host from the
// inherited environment and reaches a real herdr. Tests that need the herdr host set
// HERDR_ENV themselves with a fake herdr first on PATH. It also keeps every git the tests
// start from running background maintenance.
//
// Started as `<test binary> __pgrun -- <command>`, the binary acts as mill's pgrun helper
// instead of running the tests: the herdr host's default pgrun prefix is the running binary
// with `__pgrun --`, which under `go test` is this test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == pgrunCommand {
		os.Exit(Run(context.Background(), Env{Stdout: os.Stdout, Stderr: os.Stderr}, os.Args[1:]))
	}
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
