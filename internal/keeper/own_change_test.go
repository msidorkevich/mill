package keeper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// extraPathBuilder builds the published tree as publishing does, then adds the file name to
// it: a wrong tree, for the path check to catch.
func extraPathBuilder(name string) func(ctx context.Context, dir, start, head, parent string) (string, []string, error) {
	return func(ctx context.Context, dir, start, head, parent string) (string, []string, error) {
		tree, conflicts, err := gitx.ApplyTree(ctx, dir, start, head, parent)
		if err != nil || len(conflicts) > 0 {
			return tree, conflicts, err
		}
		ls, err := gitx.Run(ctx, dir, "ls-tree", tree)
		if err != nil {
			return "", nil, err
		}
		blob, err := gitx.RunInput(ctx, dir, "extra\n", "hash-object", "-w", "--stdin")
		if err != nil {
			return "", nil, err
		}
		tree, err = gitx.RunInput(ctx, dir, ls+"\n100644 blob "+blob+"\t"+name+"\n", "mktree")
		return tree, nil, err
	}
}

const childID = "g1/b"

// stackedOnPublished extends setup: g1/a is published and g1/b, stacked on it, changes only
// readme.txt on a branch off a's head, is verified and approved.
func stackedOnPublished(t *testing.T) *env {
	t.Helper()
	e := setupWith(t, "true", runner.Script{}, taskspec.Spec{ID: "b", Title: "Task b", Kind: "implement", Spec: "write the readme",
		Acceptance: []string{"readme exists"}, Owns: []string{"readme.txt"}, Size: "S", DependsOn: []string{"a"}})
	ctx := context.Background()
	s := e.o.Store
	if _, err := publish.Publish(ctx, publish.Options{Store: s, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "b")
	name := "mill/" + childID
	git(t, e.root, "worktree", "add", "-q", "-b", name, wt, e.head)
	head := writeCommit(t, wt, "readme.txt", "readme\n", "work on b")
	if err := s.SaveBranch(ctx, store.Branch{TaskID: childID, Name: name, Base: "mill/" + taskID, BaseSHA: e.head, StartSHA: e.head,
		HeadSHA: head, GateSHA: head, Worktree: wt}); err != nil {
		t.Fatal(err)
	}
	if aid, err := s.StartAttempt(ctx, childID, 1, "implementer"); err != nil {
		t.Fatal(err)
	} else if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "task-verdict", "g1", childID, map[string]any{"attempt": 1, "head": head,
		"verdict": map[string]any{"verdict": "pass", "summary": "fine", "findings": []any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, childID, store.TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveTask(ctx, childID, 1); err != nil {
		t.Fatal(err)
	}
	return e
}

// The incident: the base gains a change to a file the child does not own, the parent is
// restacked onto it in the pass, and the queued child, whose branch predates that change, is
// published in the same pass.
func TestQueuedChildOfParentRestackedInSamePassPublishesOnlyOwnChange(t *testing.T) {
	t.Parallel()
	e := stackedOnPublished(t)
	ctx := context.Background()
	s := e.o.Store
	e.moveMain(t, "config.txt", "[models]\n")
	tip := e.moveMain(t, "shared.txt", mainSide)
	// The parent's conflict is resolved keeping its own line, so the child's branch still
	// merges cleanly with the parent's new head: nothing but the stale-stack guard makes the
	// pass restack it.
	st, err := gitx.RebaseStart(ctx, e.wt, tip, e.base, true)
	if err != nil || st.Done {
		t.Fatalf("rebase = %+v, %v; want a stop", st, err)
	}
	if err := os.WriteFile(filepath.Join(e.wt, "shared.txt"), []byte(taskSide), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err = gitx.RebaseContinue(ctx, e.wt, st.Files, true); err != nil || !st.Done {
		t.Fatalf("continue = %+v, %v", st, err)
	}
	git(t, e.wt, "reset", "-q", "--hard", e.head)

	res := e.pass(t)
	if len(res.Queue.Published) != 1 || res.Queue.Published[0].TaskID != childID || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\nlog:\n%s", res, e.out)
	}
	ab, _ := s.GetBranch(ctx, taskID)
	bb, _ := s.GetBranch(ctx, childID)
	pub := git(t, e.bare, "rev-parse", "refs/heads/review/"+childID)
	if pub != bb.PublishedSHA {
		t.Fatalf("remote review/%s = %s, recorded %s", childID, pub, bb.PublishedSHA)
	}
	if parent := git(t, e.root, "rev-parse", pub+"^"); parent != ab.PublishedSHA {
		t.Errorf("child published on %s, want the parent's new published commit %s", parent, ab.PublishedSHA)
	}
	if got := git(t, e.root, "diff", "--name-only", pub+"^", pub); got != "readme.txt" {
		t.Errorf("child's published commit changes %q, want only readme.txt", got)
	}
	if got := git(t, e.root, "show", pub+":config.txt"); got != "[models]" {
		t.Errorf("config.txt in the child's published commit = %q, want the base's change", got)
	}
	// The child was restacked onto the parent's new head before it was published.
	if in, err := gitx.IsAncestor(ctx, e.root, ab.HeadSHA, bb.HeadSHA); err != nil || !in {
		t.Errorf("child head %s does not hold the parent's head %s (%v)", bb.HeadSHA, ab.HeadSHA, err)
	}
	restacked := false
	for _, r := range res.Rebased {
		restacked = restacked || (r.TaskID == childID && r.OntoSHA == ab.HeadSHA)
	}
	if !restacked {
		t.Errorf("rebased = %+v, want %s restacked onto %s", res.Rebased, childID, ab.HeadSHA)
	}
}

func TestQueuePublishOutsideOwnChangeIsRefusedAndPushesNothing(t *testing.T) {
	// Serial: replaces publish's process-wide tree builder (publish.SetTreeBuilder).
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	if err := e.o.Store.ApproveTask(ctx, taskID, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publish.SetTreeBuilder(extraPathBuilder("extra.txt")))

	res := e.pass(t)
	if len(res.Queue.Published) != 0 || len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0].Reason, "extra.txt") {
		t.Fatalf("result = %+v\nlog:\n%s", res, e.out)
	}
	if _, err := gitx.Run(ctx, e.bare, "rev-parse", "--verify", "-q", "refs/heads/review/"+taskID); err == nil {
		t.Error("refused publish pushed the review branch")
	}
}

func TestRepublishAfterRestackOutsideOwnChangeIsRefusedAndPushesNothing(t *testing.T) {
	// Serial: replaces publish's process-wide tree builder (publish.SetTreeBuilder).
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	pr, err := publish.Publish(ctx, publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)
	t.Cleanup(publish.SetTreeBuilder(extraPathBuilder("extra.txt")))

	res := e.pass(t)
	if len(res.Rebased) != 1 || len(res.Republished) != 0 || len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0].Reason, "extra.txt") {
		t.Fatalf("result = %+v\nlog:\n%s", res, e.out)
	}
	if got := git(t, e.bare, "rev-parse", "refs/heads/review/"+taskID); got != pr.SHA {
		t.Errorf("remote review branch moved to %s, want it still at %s", got, pr.SHA)
	}
}
