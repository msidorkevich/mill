//go:build unix

package host

import (
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKillGroupFailsOnlyWhenALiveMemberCannotBeSignalled(t *testing.T) {
	t.Parallel()
	psFailed := errors.New("ps failed")
	members := func(pids []int, err error) func(int) ([]int, error) {
		return func(int) ([]int, error) { return pids, err }
	}
	for _, tc := range []struct {
		name    string
		killErr error
		members func(int) ([]int, error)
		wantErr []error // nil means success
		want    string
	}{
		{name: "signalled"},
		{name: "group gone", killErr: syscall.ESRCH},
		{name: "only zombies or gone", killErr: syscall.EPERM, members: members(nil, nil)},
		{name: "live member", killErr: syscall.EPERM, members: members([]int{43}, nil), wantErr: []error{syscall.EPERM}, want: "process group 42"},
		{name: "cannot check", killErr: syscall.EPERM, members: members(nil, psFailed), wantErr: []error{syscall.EPERM, psFailed}, want: "process group 42"},
		{name: "no member check on this platform", killErr: syscall.EPERM, wantErr: []error{syscall.EPERM}},
		{name: "other error", killErr: syscall.EINVAL, members: members(nil, nil), wantErr: []error{syscall.EINVAL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kill := func(pid int, sig syscall.Signal) error {
				if pid != -42 || sig != syscall.SIGKILL {
					t.Errorf("kill(%d, %v), want kill(-42, SIGKILL)", pid, sig)
				}
				return tc.killErr
			}
			err := killGroup(42, kill, tc.members)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			for _, want := range tc.wantErr {
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want it to wrap %v", err, want)
				}
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestKillGroupAcceptsAGroupWithAZombieLeader(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS refuses to signal a group of zombies")
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill() // best effort after a failed test
			_ = cmd.Wait()
		}
	})
	if pids, err := groupLiveMembers(pgid); err != nil || !slices.Equal(pids, []int{pgid}) {
		t.Fatalf("live members = %v, %v, want [%d]", pids, err, pgid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Not waited for yet, so the leader stays a zombie.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pgid)).Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never became a zombie: state %q", pgid, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill of a zombie group = %v, want EPERM", err)
	}
	if err := KillGroup(pgid); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	reaped = true
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) {
		t.Fatalf("Wait = %v, want the kill signal", err)
	}
}
