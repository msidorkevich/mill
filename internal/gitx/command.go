package gitx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// gracePeriod is how long a cancelled git has after SIGTERM to exit before os/exec kills it.
// Tests lower it.
var gracePeriod = 2 * time.Second

// Command builds a git command for Bin(). When ctx is done, git gets SIGTERM, on which it
// removes its index.lock, HEAD.lock and ref locks; a git still running gracePeriod later is
// killed. Callers set Dir, Env, Stdin and the outputs.
func Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, Bin(), args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracePeriod
	return cmd
}

// withCtxErr returns err, wrapping ctx.Err() into it when ctx is done and err does not
// already report that, so callers can test for context.Canceled or DeadlineExceeded.
func withCtxErr(ctx context.Context, err error) error {
	cerr := ctx.Err()
	if err == nil || cerr == nil || errors.Is(err, cerr) {
		return err
	}
	return fmt.Errorf("%w (%w)", err, cerr)
}
