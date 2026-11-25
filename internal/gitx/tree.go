package gitx

import (
	"context"
	"fmt"
)

// HeadTree returns the tree id of HEAD in the worktree dir: what a gate run there checks,
// when the worktree has no uncommitted changes.
func HeadTree(ctx context.Context, dir string) (string, error) {
	tree, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{tree}")
	if err != nil {
		return "", fmt.Errorf("read the tree of HEAD in %s: %w", dir, err)
	}
	return tree, nil
}
