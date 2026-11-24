package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/store"
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

func TestApproveRefusesPublishOutsideOwnChangeAndPushesNothing(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	t.Cleanup(publish.SetTreeBuilder(extraPathBuilder("extra.txt")))

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code == ExitOK {
		t.Fatalf("approve succeeded: %+v", r)
	}
	if !strings.Contains(r.stderr, "extra.txt") || !strings.Contains(r.stderr, "own change") {
		t.Errorf("stderr does not name the extra path:\n%s", r.stderr)
	}
	if after := remoteRefs(t, bare); len(after) != len(before) || after["refs/heads/review/g1/a"] != "" {
		t.Errorf("remote refs changed: %v -> %v", before, after)
	}
	s := openStore(t, root)
	tk, _ := s.GetTask(context.Background(), "g1/a")
	br, _ := s.GetBranch(context.Background(), "g1/a")
	if tk.Status != store.TaskVerified || br.PublishedSHA != "" {
		t.Errorf("task %s, published %q; want verified and unpublished", tk.Status, br.PublishedSHA)
	}
}
