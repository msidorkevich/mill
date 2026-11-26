package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runner"
)

// recordingRunner passes every request to the wrapped runner and records it.
type recordingRunner struct {
	runner.Runner
	mu   sync.Mutex
	reqs []runner.Request
}

func (r *recordingRunner) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.Runner.Run(ctx, req)
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Not parallel: it sets the package-level wrapPlanRunner hook.
func TestPlanGoalTextPassesTmpDirUnderWorktreeRootOutsideRepo(t *testing.T) {
	root := plannerRepo(t, validPlan)
	wtRoot := t.TempDir()
	paths := project.PathsAt(root)
	cfg := "check_cmd = \"true\"\nworktree_root = \"" + wtRoot + "\"\n[runner]\nkind = \"fake\"\nfake_script = \"script.json\"\n"
	if err := os.WriteFile(paths.Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recordingRunner{}
	wrapPlanRunner = func(r runner.Runner) runner.Runner { rec.Runner = r; return rec }
	t.Cleanup(func() { wrapPlanRunner = nil })

	home := t.TempDir()
	r := runEnv(t, root, Env{Home: home}, "plan", "add a greeting")
	if r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if len(rec.reqs) != 1 {
		t.Fatalf("planner requests = %d, want 1", len(rec.reqs))
	}
	req := rec.reqs[0]
	logs, err := filepath.Glob(filepath.Join(paths.Runs, "plans", "*"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("plan log dirs: %v %v", logs, err)
	}
	loaded, err := config.Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(loaded.Worktrees(paths.Root, home), ".tmp", "plan-"+filepath.Base(logs[0]))
	if req.TmpDir != want {
		t.Errorf("TmpDir = %q, want %q", req.TmpDir, want)
	}
	if !within(req.TmpDir, wtRoot) {
		t.Errorf("TmpDir %q is not under the worktree root %q", req.TmpDir, wtRoot)
	}
	for _, dir := range []string{root, paths.MillDir} {
		if within(req.TmpDir, dir) {
			t.Errorf("TmpDir %q lies inside %q", req.TmpDir, dir)
		}
	}
}
