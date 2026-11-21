//go:build unix

package host

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessInfo reports the process group and full command line of pid, and whether it is
// alive. A missing process is alive=false with no error; a failure to ask is an error.
func ProcessInfo(pid int) (pgid int, command string, alive bool, err error) {
	if pid <= 0 {
		return 0, "", false, nil
	}
	out, err := exec.Command("ps", "-o", "pgid=,command=", "-p", strconv.Itoa(pid)).Output()
	line := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		// ps exits 1 with no output when the pid does not exist.
		if errors.As(err, &ee) && ee.ExitCode() == 1 && line == "" {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	if line == "" {
		return 0, "", false, nil
	}
	f := strings.Fields(line)
	pgid, err = strconv.Atoi(f[0])
	if err != nil {
		return 0, "", false, fmt.Errorf("ps -p %d: unexpected output %q", pid, line)
	}
	return pgid, strings.TrimSpace(strings.TrimPrefix(line, f[0])), true, nil
}
