// Package testenv holds helpers that keep tests from leaving work behind: git that starts
// no background maintenance, and waiting shells that end on their own and are killed with
// their test.
package testenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// gitSettings are the git config entries DisableGitMaintenance adds: no automatic
// maintenance and no automatic gc, so no git run by a test leaves a detached process behind.
var gitSettings = [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}}

// DisableGitMaintenance turns off git's automatic maintenance and gc for every git the
// process starts from now on, through GIT_CONFIG_COUNT and GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n.
// Entries already in the environment are kept; the settings are appended after them. Call it
// from TestMain before m.Run.
func DisableGitMaintenance() error {
	n := 0
	if v, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok && v != "" {
		c, err := strconv.Atoi(v)
		if err != nil || c < 0 {
			return fmt.Errorf("GIT_CONFIG_COUNT=%q is not a count: unset it or set it to a non-negative number", v)
		}
		n = c
	}
	for _, kv := range gitSettings {
		if err := os.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(n), kv[0]); err != nil {
			return fmt.Errorf("set GIT_CONFIG_KEY_%d: %w", n, err)
		}
		if err := os.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(n), kv[1]); err != nil {
			return fmt.Errorf("set GIT_CONFIG_VALUE_%d: %w", n, err)
		}
		n++
	}
	if err := os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n)); err != nil {
		return fmt.Errorf("set GIT_CONFIG_COUNT: %w", err)
	}
	return nil
}

// WaitLimitSeconds is how long a WaitForFileScript loop waits before it gives up.
const WaitLimitSeconds = 60

// WaitForFileScript returns a POSIX shell snippet that polls every 10ms until any of the
// files names in dir exists, then continues with the rest of the script. It gives up and
// exits with status 1 after WaitLimitSeconds, or as soon as dir is gone (its test's temporary
// directory removed), so a shell its test left behind ends on its own.
func WaitForFileScript(dir string, names ...string) string {
	var conds []string
	for _, n := range names {
		conds = append(conds, "[ ! -e "+Quote(filepath.Join(dir, n))+" ]")
	}
	return "testenv_end=$(( $(date +%s) + " + strconv.Itoa(WaitLimitSeconds) + " )); " +
		"while " + strings.Join(conds, " && ") + "; do " +
		"if [ ! -d " + Quote(dir) + " ] || [ \"$(date +%s)\" -ge \"$testenv_end\" ]; then exit 1; fi; " +
		"sleep 0.01; done"
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Groups starts shell snippets in their own process groups and kills those groups when its
// test ends.
type Groups struct{ dir string }

// NewGroups returns a Groups whose process groups are killed in t.Cleanup, before the
// temporary directories the test created earlier are removed.
func NewGroups(t testing.TB) *Groups {
	t.Helper()
	g := &Groups{dir: t.TempDir()}
	t.Cleanup(func() {
		if err := g.kill(); err != nil {
			t.Error(err)
		}
	})
	return g
}

// Script wraps body so it runs in its own process group without job control: the shell
// starts sh -c body in the background through the pgrun helper (PgrunPrefix), which makes it
// the leader of a new group and execs in place, so its pid ($! to the wrapper) is the group
// id. That sh records its pid ($$, equal to $!) before running body; it does so itself
// because only then does the group exist, so a kill at cleanup never finds a recorded group
// missing while the body is still to start. The wrapper waits for body and exits with its
// status.
func (g *Groups) Script(body string) string {
	inner := ": > " + Quote(g.dir) + "/$$; " + body
	return PgrunPrefix() + " sh -c " + Quote(inner) + " & wait $!"
}

// kill sends SIGKILL to every process group a Script recorded.
func (g *Groups) kill() error {
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return fmt.Errorf("list the test's process groups in %s: %w", g.dir, err)
	}
	var errs []error
	for _, e := range entries {
		pgid, err := strconv.Atoi(e.Name())
		if err != nil || pgid <= 1 {
			errs = append(errs, fmt.Errorf("%s in %s is not a process group id", e.Name(), g.dir))
			continue
		}
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !groupGone(err) {
			errs = append(errs, fmt.Errorf("kill process group %d: %w: stop it by hand", pgid, err))
		}
	}
	return errors.Join(errs...)
}

// groupGone reports whether a kill error means the group has no live member. The groups
// are the test's own processes, so EPERM only comes from macOS, where kill on a group
// whose members have all exited but are not yet reaped fails with EPERM.
func groupGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}
