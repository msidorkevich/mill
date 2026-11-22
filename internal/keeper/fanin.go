package keeper

import (
	"context"

	"github.com/msidorkevich/mill/internal/store"
)

// restackFanIn keeps a task with several dependencies on top of them. It is not handled yet:
// the task is reported as skipped and its branch is left alone.
func (k *keeper) restackFanIn(ctx context.Context, t store.Task, br store.Branch) error {
	k.skip(t.ID, "fan-in: not handled yet")
	return nil
}
