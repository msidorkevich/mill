//go:build unix

package host

import (
	"errors"
	"fmt"
	"syscall"
)

// KillGroup sends SIGKILL to the whole process group. A group that is already gone, or
// that has no live member left, is not an error.
func KillGroup(pgid int) error {
	return killGroup(pgid, syscall.Kill, groupLiveMembers)
}

// killGroup is KillGroup with the system calls injected. liveMembers returns the pids in
// the group that are still alive; it is consulted when kill fails with EPERM, and is nil on
// platforms where EPERM always means a process could not be signalled.
func killGroup(pgid int, kill func(int, syscall.Signal) error, liveMembers func(int) ([]int, error)) error {
	err := kill(-pgid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if !errors.Is(err, syscall.EPERM) || liveMembers == nil {
		return err
	}
	pids, checkErr := liveMembers(pgid)
	if checkErr != nil {
		return fmt.Errorf("kill process group %d: %w, and its members could not be checked: %w: look for them with ps and stop them by hand", pgid, err, checkErr)
	}
	if len(pids) == 0 {
		return nil
	}
	return fmt.Errorf("kill process group %d: %w: pids %v are still alive: check their owner with ps and stop them by hand", pgid, err, pids)
}
