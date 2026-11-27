package testenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// startRetries is how many times StartRetry starts a command again after ETXTBSY, and
// startRetryWait is how long it waits before each new attempt.
const (
	startRetries   = 5
	startRetryWait = 5 * time.Millisecond
)

// tempSeq makes the temporary names WriteExecutable uses unique within the process.
var tempSeq atomic.Uint64

// WriteExecutable writes body to path as an executable (mode 0o755) that can be run at once,
// even while other goroutines fork. Linux refuses to exec a file that any process holds open
// for writing (ETXTBSY), and a child forked by another goroutine inherits every descriptor
// open at the moment of the fork until it execs. So the file is created with O_CLOEXEC under a
// temporary name in the same directory, written and closed while syscall.ForkLock is held for
// reading (os/exec takes it for writing around each fork), and only then renamed into place.
// No descriptor open for writing on the file can leak into a child. Failures end the test.
func WriteExecutable(t testing.TB, path string, body []byte) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path),
		"."+filepath.Base(path)+".tmp-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(tempSeq.Add(1), 10))
	if err := writeClosed(tmp, body); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			t.Errorf("remove temporary file %s: %v", tmp, rmErr)
		}
		t.Fatalf("write executable %s: %v", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil {
			t.Errorf("remove temporary file %s: %v", tmp, rmErr)
		}
		t.Fatalf("move executable into place at %s: %v", path, err)
	}
}

// writeClosed creates name with mode 0o755, writes body and closes it, all under
// syscall.ForkLock, so no fork happens while the descriptor is open.
func writeClosed(name string, body []byte) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_CLOEXEC, 0o700)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	// Chmod on the descriptor sets 0o755 exactly, whatever the umask.
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// StartRetry starts cmd like cmd.Start, and starts it again when the exec fails with
// ETXTBSY (the file is still open for writing somewhere, typically in a child another
// goroutine forked a moment earlier and that has not yet exec'd). An exec.Cmd cannot be
// started twice, so before each retry *cmd is rebuilt from its exported fields (Path, Args,
// Env, Dir, standard streams, ExtraFiles, SysProcAttr, Cancel, WaitDelay); a context given
// to exec.CommandContext is not carried over. It retries up to 5 times, waiting a few
// milliseconds before each. Any other error is returned at once, naming the path; when every
// retry still meets ETXTBSY the error names the path and says the file is busy. After a nil
// return, call cmd.Wait as usual.
func StartRetry(cmd *exec.Cmd) error {
	for attempt := 0; ; attempt++ {
		err := cmd.Start()
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.ETXTBSY) {
			return fmt.Errorf("start %s: %w", cmd.Path, err)
		}
		if attempt == startRetries {
			return fmt.Errorf("start %s: the file is busy (still open for writing) after %d retries: %w",
				cmd.Path, startRetries, err)
		}
		time.Sleep(startRetryWait)
		*cmd = exec.Cmd{
			Path:        cmd.Path,
			Args:        cmd.Args,
			Env:         cmd.Env,
			Dir:         cmd.Dir,
			Stdin:       cmd.Stdin,
			Stdout:      cmd.Stdout,
			Stderr:      cmd.Stderr,
			ExtraFiles:  cmd.ExtraFiles,
			SysProcAttr: cmd.SysProcAttr,
			Cancel:      cmd.Cancel,
			WaitDelay:   cmd.WaitDelay,
		}
	}
}
