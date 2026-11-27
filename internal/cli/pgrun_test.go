package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPgrunIsHiddenFromHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var out bytes.Buffer
		code := Run(context.Background(), Env{Stdout: &out, Stderr: &out, Dir: t.TempDir()}, args)
		if code != ExitOK {
			t.Fatalf("mill %s: exit %d", args[0], code)
		}
		if strings.Contains(out.String(), "pgrun") {
			t.Errorf("mill %s lists the hidden helper:\n%s", args[0], out.String())
		}
	}
	var out bytes.Buffer
	Run(context.Background(), Env{Stdout: &out, Stderr: &out, Dir: t.TempDir()}, nil)
	if strings.Contains(out.String(), "pgrun") {
		t.Errorf("usage lists the hidden helper:\n%s", out.String())
	}
}

func TestPgrunMissingCommandFailsOutsideRepository(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir() // not a git repository: the helper must not need one
	code := Run(context.Background(), Env{Stdout: &stdout, Stderr: &stderr, Dir: dir, Home: t.TempDir()},
		[]string{"__pgrun", "--", "/nonexistent"})
	if code != 127 {
		t.Errorf("exit %d, want 127", code)
	}
	msg := stderr.String()
	if !strings.HasPrefix(msg, "mill __pgrun: ") || !strings.Contains(msg, "/nonexistent") {
		t.Errorf("stderr = %q, want a mill __pgrun error naming /nonexistent", msg)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

func TestPgrunWithoutCommandFails(t *testing.T) {
	var stderr bytes.Buffer
	code := Run(context.Background(), Env{Stdout: &stderr, Stderr: &stderr, Dir: t.TempDir()}, []string{"__pgrun"})
	if code != 126 {
		t.Errorf("exit %d, want 126", code)
	}
	if !strings.Contains(stderr.String(), "no command") {
		t.Errorf("stderr = %q, want a no-command error", stderr.String())
	}
}
