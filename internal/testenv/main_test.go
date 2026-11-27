package testenv

import (
	"os"
	"testing"
)

// TestMain keeps every git the tests start from running background maintenance.
func TestMain(m *testing.M) {
	if err := DisableGitMaintenance(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
