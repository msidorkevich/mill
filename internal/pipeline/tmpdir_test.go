package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
)

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestWorkerTmpDirIsUnderWorktreeRootOutsideRepoAndMill(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "tmpd")
	e.run(t, "tmpd")
	calls := e.fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	base := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), ".tmp", "tmpd")
	for _, c := range calls {
		want := filepath.Join(base, fmt.Sprintf("a1-%s", c.Role))
		if c.TmpDir != want {
			t.Errorf("%s TmpDir = %q, want %q", c.Role, c.TmpDir, want)
		}
		if !within(c.TmpDir, e.p.Cfg.WorktreeRoot) {
			t.Errorf("%s TmpDir %s is not under the worktree root %s", c.Role, c.TmpDir, e.p.Cfg.WorktreeRoot)
		}
		if within(c.TmpDir, e.root) {
			t.Errorf("%s TmpDir %s is inside the repository %s", c.Role, c.TmpDir, e.root)
		}
		if within(c.TmpDir, e.p.Paths.MillDir) {
			t.Errorf("%s TmpDir %s is inside .mill %s", c.Role, c.TmpDir, e.p.Paths.MillDir)
		}
		for _, p := range c.ProtectedPaths {
			if within(c.TmpDir, p) {
				t.Errorf("%s TmpDir %s is inside protected path %s", c.Role, c.TmpDir, p)
			}
		}
	}
}
