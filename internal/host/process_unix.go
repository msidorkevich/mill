//go:build unix

package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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

// ProcessCwd returns the current working directory of pid. A process that is gone or whose
// directory cannot be read is an error.
func ProcessCwd(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("process cwd: invalid pid %d", pid)
	}
	if runtime.GOOS == "linux" {
		cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			return "", fmt.Errorf("process cwd of %d: %w", pid, err)
		}
		return cwd, nil
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", fmt.Errorf("lsof -p %d: %w", pid, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:], nil
		}
	}
	return "", fmt.Errorf("lsof -p %d: no working directory in output %q", pid, strings.TrimSpace(string(out)))
}

// KillProcess sends SIGKILL to the single process pid. A process that is already gone is
// not an error.
func KillProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
