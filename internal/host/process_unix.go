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

// GroupMembers returns the pids of live processes whose process group id is pgid. A group
// that does not exist has no members and is not an error; a failure to ask is an error.
func GroupMembers(pgid int) ([]int, error) {
	if pgid <= 0 {
		return nil, nil
	}
	out, err := exec.Command("ps", "-ax", "-o", "pid=,pgid=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps -ax: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("ps -ax: unexpected output %q", line)
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("ps -ax: unexpected output %q", line)
		}
		if g == pgid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
