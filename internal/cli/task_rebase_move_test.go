package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// moveTask runs moveTaskOntoBase for task id in the repository at root.
func moveTask(t *testing.T, root, id string) (moveResult, error) {
	t.Helper()
	ctx := context.Background()
	paths := project.PathsAt(root)
	cfg, err := config.Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, root)
	task, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return moveTaskOntoBase(ctx, s, cfg, paths, task)
}

func TestMoveTaskOntoBaseReportsMovedCommits(t *testing.T) {
	t.Parallel()
	root := soloRepo(t, `{"solo.txt": "s\n"}`)
	before, _ := openStore(t, root).GetBranch(context.Background(), "solo")
	tip := commitOnMain(t, root, "m.txt", "m\n")

	mr, err := moveTask(t, root, "solo")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := openStore(t, root).GetBranch(context.Background(), "solo")
	cfg, err := config.Load(project.PathsAt(root).Config)
	if err != nil {
		t.Fatal(err)
	}
	if mr.Onto != cfg.BaseBranch || mr.OntoSHA != tip || mr.OldStart != before.StartSHA || mr.NewStart != tip ||
		mr.OldHead != before.HeadSHA || mr.NewHead != after.HeadSHA || mr.UpToDate || mr.Restacked {
		t.Errorf("result = %+v, before %+v, after %+v, tip %s", mr, before, after, tip)
	}
	again, err := moveTask(t, root, "solo")
	if err != nil || !again.UpToDate {
		t.Errorf("second move = %+v, %v", again, err)
	}
}

func TestMoveTaskOntoBaseConflictIsTypedAndLeavesBranchUnchanged(t *testing.T) {
	t.Parallel()
	root := soloRepo(t, `{"clash.txt": "task\n"}`)
	ctx := context.Background()
	before, _ := openStore(t, root).GetBranch(ctx, "solo")
	commitOnMain(t, root, "clash.txt", "main\n")

	mr, err := moveTask(t, root, "solo")
	var ce *moveConflictError
	if !errors.As(err, &ce) || !reflect.DeepEqual(ce.Files, []string{"clash.txt"}) || !reflect.DeepEqual(mr.ConflictFiles, ce.Files) {
		t.Fatalf("move = %+v, %v; want a conflict in clash.txt", mr, err)
	}
	after, _ := openStore(t, root).GetBranch(ctx, "solo")
	if after != before {
		t.Errorf("row changed: %+v -> %+v", before, after)
	}
	if head := gitC(t, before.Worktree, "rev-parse", "HEAD"); head != before.HeadSHA {
		t.Errorf("head = %s, want %s", head, before.HeadSHA)
	}
}

func TestMoveTaskOntoBaseFanInConflictCarriesStep(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	ctx := context.Background()
	s := openStore(t, root)
	b, _ := s.GetBranch(ctx, "g1/b")
	before, _ := s.GetBranch(ctx, "g1/c")
	if err := os.WriteFile(filepath.Join(b.Worktree, "c.txt"), []byte("from b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, b.Worktree, "add", "c.txt")
	gitC(t, b.Worktree, "commit", "-q", "-m", "b writes c.txt")

	mr, err := moveTask(t, root, "g1/c")
	var ce *moveConflictError
	if !errors.As(err, &ce) || !mr.Restacked || ce.Step == "" || mr.ConflictStep != ce.Step || !reflect.DeepEqual(ce.Files, []string{"c.txt"}) {
		t.Fatalf("move = %+v, %v; want a fan-in conflict in c.txt", mr, err)
	}
	if after, _ := s.GetBranch(ctx, "g1/c"); after != before {
		t.Errorf("row changed: %+v -> %+v", before, after)
	}
	if head := gitC(t, before.Worktree, "rev-parse", "HEAD"); head != before.HeadSHA {
		t.Errorf("head = %s, want %s", head, before.HeadSHA)
	}
}

func TestMoveTaskOntoBaseReportsDependencyNotReady(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, chainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	if r := groupBRun(t, root, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	setStatus(t, root, "g2/a", store.TaskPending)

	_, err := moveTask(t, root, "g2/b")
	var dn *depNotReadyError
	if !errors.As(err, &dn) || !strings.Contains(err.Error(), "not verified, published or landed; run it first") {
		t.Fatalf("err = %v; want a dependency-not-ready error", err)
	}
	var ce *moveConflictError
	if errors.As(err, &ce) {
		t.Errorf("not-ready error is also a conflict: %v", err)
	}
}

func TestMoveTaskOntoBaseFanInReportsDependencyNotReady(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := groupBRun(t, root, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	setStatus(t, root, "g1/a", store.TaskPending)

	_, err := moveTask(t, root, "g1/c")
	var dn *depNotReadyError
	if !errors.As(err, &dn) || !strings.Contains(err.Error(), "cannot be rebased yet") {
		t.Fatalf("err = %v; want a dependency-not-ready error", err)
	}
}
