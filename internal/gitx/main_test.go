package gitx

import (
	"os"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// TestMain keeps every git the tests start from running background maintenance.
func TestMain(m *testing.M) {
	if err := testenv.DisableGitMaintenance(); err != nil {
		panic(err)
	}
	if os.Getenv(wtAdminHelperEnv) != "" {
		os.Exit(wtAdminHelperMain())
	}
	os.Exit(m.Run())
}
