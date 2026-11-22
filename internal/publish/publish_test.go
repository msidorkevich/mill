package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fixture is a repo with a bare remote and a verified single task "t1" on branch mill/t/t1.
func fixture(t *testing.T) (Options, string, store.Branch) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README")
	git(t, root, "commit", "-q", "-m", "init")
	base := git(t, root, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")

	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", "-b", "mill/t/t1", wt, base)
	if err := os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "x.txt")
	git(t, wt, "commit", "-q", "-m", "work 1")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "work 2")
	head := git(t, wt, "rev-parse", "HEAD")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	spec := taskspec.Spec{ID: "t1", Title: "Write x", Kind: "implement", Spec: "write x", Acceptance: []string{"x exists"},
		Checks: []string{"test -f x.txt"}, Size: "S"}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	br := store.Branch{TaskID: "t1", Name: "mill/t/t1", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: head, GateSHA: head, Worktree: wt}
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "task-verdict", "", "t1", map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "t1", store.TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	return Options{Store: s, Cfg: cfg, Paths: paths, TaskID: "t1"}, bare, br
}

func TestPublishPushesSquashedCommitOnBase(t *testing.T) {
	o, bare, br := fixture(t)
	mainBefore := git(t, bare, "rev-parse", "refs/heads/main")
	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "review/t1" || res.Parent != br.BaseSHA || res.Onto != "main" || res.Republish || res.GateRerun != nil {
		t.Errorf("result = %+v", res)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/review/t1"); got != res.SHA {
		t.Errorf("remote review/t1 = %s, want %s", got, res.SHA)
	}
	if got := git(t, bare, "rev-list", "--count", br.BaseSHA+".."+res.SHA); got != "1" {
		t.Errorf("%s commits on top of the base, want 1", got)
	}
	msg := git(t, bare, "log", "-1", "--format=%B", res.SHA)
	if !strings.HasPrefix(msg, "Write x\n") || strings.Contains(msg, "Mill-Goal") || !strings.Contains(msg, "Mill-Task: t1") ||
		!strings.Contains(msg, "Mill-Base: "+br.BaseSHA) || !strings.Contains(msg, "Verifier: pass: fine") {
		t.Errorf("message:\n%s", msg)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Errorf("remote main moved to %s", got)
	}
	tk, _ := o.Store.GetTask(context.Background(), "t1")
	after, _ := o.Store.GetBranch(context.Background(), "t1")
	if tk.Status != store.TaskPublished || after.PublishedRef != "review/t1" || after.PublishedSHA != res.SHA {
		t.Errorf("task %s, branch %+v", tk.Status, after)
	}
}

func TestPublishRefusalsAreRefusedErrors(t *testing.T) {
	o, bare, br := fixture(t)
	ctx := context.Background()

	br.GateSHA = ""
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	o.Cfg.CheckCmd = "false"
	res, err := Publish(ctx, o)
	var re *RefusedError
	if !errors.As(err, &re) || re.Rule != ruleGate || !strings.Contains(err.Error(), "false (exit 1") {
		t.Fatalf("gate refusal = %v", err)
	}
	if res.GateRerun == nil || res.GateRerun.Passed || len(res.GateRerun.Commands) != 2 {
		t.Errorf("gate re-run = %+v", res.GateRerun)
	}
	evs, _ := o.Store.TaskEvents(ctx, "t1")
	if last := evs[len(evs)-1]; last.Kind != "task-gate" {
		t.Errorf("last event = %+v, want the gate re-run", last)
	}
	if _, err := gitx.RevParse(ctx, bare, "refs/heads/review/t1"); err == nil {
		t.Error("refused publish pushed review/t1")
	}

	o.Cfg.Remote = "nowhere"
	if _, err := Publish(ctx, o); !errors.As(err, &re) || re.Rule != ruleRemote {
		t.Fatalf("remote refusal = %v", err)
	}
}

func TestParentRefusalRecognisesOnlyTheDependencyRule(t *testing.T) {
	parentErr := fmt.Errorf("wrapped: %w", &RefusedError{Rule: ruleParent, Parent: "g/a", Msg: "publish g/a first"})
	if p, ok := ParentRefusal(parentErr); !ok || p != "g/a" {
		t.Errorf("ParentRefusal(parent rule) = %q, %v", p, ok)
	}
	for _, err := range []error{&RefusedError{Rule: ruleGate, Msg: "red"}, errors.New("push failed"), nil} {
		if _, ok := ParentRefusal(err); ok {
			t.Errorf("ParentRefusal(%v) = true", err)
		}
	}
}
