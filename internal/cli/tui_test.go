package cli

import (
	"strings"
	"testing"
)

func TestHelpListsTUI(t *testing.T) {
	r := run(t, t.TempDir(), nil, "help")
	if r.code != ExitOK || !strings.Contains(r.stdout, "tui      interactive terminal UI") {
		t.Fatalf("help: %+v", r)
	}
}

func TestTUIHelpFlagExitsZero(t *testing.T) {
	r := run(t, t.TempDir(), nil, "tui", "-h")
	if r.code != ExitOK || !strings.Contains(r.stderr, "mill tui") {
		t.Fatalf("tui -h: %+v", r)
	}
}

func TestTUIOutsideProjectFails(t *testing.T) {
	r := run(t, gitRepo(t), nil, "tui")
	if r.code != ExitError || !strings.Contains(r.stderr, "mill tui:") {
		t.Fatalf("tui outside an initialized repo: %+v", r)
	}
}
