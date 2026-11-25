package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGateReuseHeadTreeIsTheTreeOfHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := repo(t)
	tree, err := HeadTree(ctx, r)
	if err != nil || len(tree) != 40 {
		t.Fatalf("HeadTree = %q, %v", tree, err)
	}
	if want := mustRun(t, ctx, r, "rev-parse", "HEAD^{tree}"); tree != want {
		t.Fatalf("HeadTree = %s, want %s", tree, want)
	}

	// A new commit with the same content (an empty commit) keeps the tree; a change does not.
	mustRun(t, ctx, r, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "empty")
	if same, err := HeadTree(ctx, r); err != nil || same != tree {
		t.Fatalf("HeadTree after an empty commit = %s, %v; want %s", same, err, tree)
	}
	if err := os.WriteFile(filepath.Join(r, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitAll(ctx, r, "x"); err != nil {
		t.Fatal(err)
	}
	if other, err := HeadTree(ctx, r); err != nil || other == tree {
		t.Fatalf("HeadTree after a change = %s, %v; want a new tree", other, err)
	}
}

func TestGateReuseHeadTreeFailsOutsideARepository(t *testing.T) {
	t.Parallel()
	if _, err := HeadTree(context.Background(), t.TempDir()); err == nil {
		t.Fatal("HeadTree outside a repository succeeded")
	}
}
