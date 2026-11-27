package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookOrderApprove approves id with one writer shared by stdout and stderr, so the output
// keeps the order the lines were written in.
func hookOrderApprove(t *testing.T, root, id string) string {
	t.Helper()
	var shared bytes.Buffer
	code := Run(context.Background(), Env{Stdout: &shared, Stderr: &shared, Dir: root}, []string{"review", "approve", id})
	if code != ExitOK {
		t.Fatalf("approve %s exited %d:\n%s", id, code, shared.String())
	}
	return shared.String()
}

// hookOrderIndex is the index of sub in s, failing the test when s lacks it.
func hookOrderIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("output lacks %q:\n%s", sub, s)
	}
	return i
}

func TestHookLineAfterPublishedOnStdout(t *testing.T) {
	t.Parallel()
	root, _ := hookRepo(t, "")
	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	published := hookOrderIndex(t, r.stdout, "Published g1/a as ")
	hookLine := hookOrderIndex(t, r.stdout, "hook on_publish for g1/a: hook said publish\n")
	next := hookOrderIndex(t, r.stdout, "Next:")
	if !(published < hookLine && hookLine < next) {
		t.Errorf("want Published, then the hook line, then Next:\n%s", r.stdout)
	}
	if n := len(hookRunEvents(t, root, "g1/a")); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}
}

func TestHookLineAfterPublishedFailureOnStderr(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "")
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run(context.Background(), Env{Stdout: &out, Stderr: &errb, Dir: root}, []string{"review", "approve", "g1/a"}); code != ExitOK {
		t.Fatalf("approve exited %d:\n%s\n%s", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "on_publish") {
		t.Errorf("stderr lacks the hook failure:\n%s", errb.String())
	}
	if strings.Contains(out.String(), "hook on_publish for") {
		t.Errorf("stdout has a hook line for a failed hook:\n%s", out.String())
	}
	if evs := hookRunEvents(t, root, "g1/a"); len(evs) != 1 || evs[0].ExitCode != 3 {
		t.Errorf("hook-run events = %+v, want one with exit code 3", evs)
	}
}

func TestHookLineAfterPublishedFailureOrderWithSharedWriter(t *testing.T) {
	t.Parallel()
	root, dir := hookRepo(t, "")
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := hookOrderApprove(t, root, "g1/a")
	published := hookOrderIndex(t, s, "Published g1/a as ")
	failure := hookOrderIndex(t, s, "on_publish")
	if published > failure {
		t.Errorf("the hook failure came before the Published line:\n%s", s)
	}
}
