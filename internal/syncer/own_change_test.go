package syncer

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/publish"
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

// publishedStack is stackedGoal with a green gate, g1/a published at its new head and g1/b
// published on a's old head, so that sync restacks b and re-publishes it.
func publishedStack(t *testing.T) (Options, string) {
	t.Helper()
	o := stackedGoal(t, false)
	o.Cfg.CheckCmd = "true"
	ctx := context.Background()
	bare, err := gitx.Run(ctx, o.Paths.Root, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1/a", "g1/b"} {
		if _, err := o.Store.AppendEvent(ctx, "task-verdict", "g1", id, map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine"}}); err != nil {
			t.Fatal(err)
		}
	}
	pub := func(id string) {
		t.Helper()
		if _, err := publish.Publish(ctx, publish.Options{Store: o.Store, Cfg: o.Cfg, Paths: o.Paths, TaskID: id}); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}
	// Publish a at the head b was based on, then b, then a again at its new head.
	ab, _ := o.Store.GetBranch(ctx, "g1/a")
	bb, _ := o.Store.GetBranch(ctx, "g1/b")
	aNew := ab.HeadSHA
	setHead := func(sha string) {
		t.Helper()
		git(t, ab.Worktree, "reset", "-q", "--hard", sha)
		ab, _ = o.Store.GetBranch(ctx, "g1/a")
		ab.HeadSHA, ab.GateSHA = sha, sha
		if err := o.Store.SaveBranch(ctx, ab); err != nil {
			t.Fatal(err)
		}
	}
	setHead(bb.BaseSHA)
	pub("g1/a")
	pub("g1/b")
	setHead(aNew)
	pub("g1/a")
	return o, bare
}

func TestSyncRepublishAfterRestackPublishesOnlyOwnChange(t *testing.T) {
	t.Parallel()
	o, bare := publishedStack(t)
	ctx := context.Background()
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || len(res.Republished) != 1 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v", res)
	}
	pub := git(t, bare, "rev-parse", "refs/heads/review/g1/b")
	ab, _ := o.Store.GetBranch(ctx, "g1/a")
	if parent := git(t, bare, "rev-parse", pub+"^"); parent != ab.PublishedSHA {
		t.Errorf("republished on %s, want %s", parent, ab.PublishedSHA)
	}
	if got := git(t, bare, "diff", "--name-only", pub+"^", pub); got != "b.txt" {
		t.Errorf("republished commit changes %q, want only b.txt", got)
	}
}

func TestSyncRepublishOutsideOwnChangeIsRefusedAndPushesNothing(t *testing.T) {
	// Serial: replaces publish's process-wide tree builder (publish.SetTreeBuilder).
	o, bare := publishedStack(t)
	ctx := context.Background()
	before := git(t, bare, "rev-parse", "refs/heads/review/g1/b")
	t.Cleanup(publish.SetTreeBuilder(extraPathBuilder("extra.txt")))
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || len(res.Republished) != 0 || len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0].Reason, "extra.txt") {
		t.Fatalf("result = %+v", res)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/review/g1/b"); got != before {
		t.Errorf("refused republish moved review/g1/b from %s to %s", before, got)
	}
}
