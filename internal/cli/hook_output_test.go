package cli

import (
	"strings"
	"testing"
)

func TestApproveHookSuccessLineGoesToStdout(t *testing.T) {
	t.Parallel()
	root, _ := hookRepo(t, "")
	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	want := "hook on_publish for g1/a: hook said publish\n"
	if n := strings.Count(r.stdout, want); n != 1 {
		t.Errorf("stdout has %d lines %q, want 1:\n%s", n, want, r.stdout)
	}
}
