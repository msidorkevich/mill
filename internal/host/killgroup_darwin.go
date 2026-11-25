package host

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// On macOS kill(2) on a process group whose remaining members have all exited but are not
// yet reaped fails with EPERM, not ESRCH, so EPERM needs a look at the members.
var groupLiveMembers = psLiveMembers

// psLiveMembers returns the pids in group pgid that are neither zombies (state Z) nor on
// their way out (flag E); neither can be signalled or run again.
func psLiveMembers(pgid int) ([]int, error) {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,pgid=,stat=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps -ax: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("ps -ax: unexpected output %q", line)
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("ps -ax: unexpected output %q", line)
		}
		if g == pgid && !strings.HasPrefix(f[2], "Z") && !strings.Contains(f[2], "E") {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
