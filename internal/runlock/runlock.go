// Package runlock enforces one scheduler per repository: whoever holds the lock is the
// scheduler for that repository, and Held lets other processes probe for it.
//
// The lock is an fcntl write lock (F_SETLK) over the whole file. fcntl locks belong to the
// process, not to the open file, so a child that is still starting up and shares mill's open
// files never keeps the lock alive: it ends exactly when mill ends.
//
// fcntl locks have two traps, both handled here: locks do not conflict inside one process
// (the registry below rejects a second Acquire of the same path), and closing any descriptor
// of the file releases the lock (the file is never reopened while this process holds it).
package runlock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	heldMu sync.Mutex
	held   = map[string]bool{}
)

// Lock is a held run lock.
type Lock struct {
	f    *os.File
	path string
}

func setLock(f *os.File, typ int16) error {
	fl := syscall.Flock_t{Type: typ, Whence: 0, Start: 0, Len: 0}
	return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &fl)
}

// Acquire takes the lock on path without blocking, creating the file, and records the
// current pid in it. When another process holds the lock the error names that process.
func Acquire(path string) (*Lock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve run lock path %s: %w", path, err)
	}
	abs = filepath.Clean(abs)
	heldMu.Lock()
	defer heldMu.Unlock()
	if held[abs] {
		return nil, fmt.Errorf("another mill run is active (pid %d, this process already holds run lock %s); wait for it to finish", os.Getpid(), abs)
	}
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run lock %s: %w", abs, err)
	}
	if err := setLock(f, syscall.F_WRLCK); err != nil {
		// Contention means another process holds the lock, so reading the file here cannot
		// close a descriptor of a lock this process owns (the registry checked that above).
		holder := "unknown pid"
		if data, rerr := os.ReadFile(abs); rerr == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				holder = "pid " + s
			}
		}
		f.Close()
		if err == syscall.EAGAIN || err == syscall.EACCES {
			return nil, fmt.Errorf("another mill run is active (%s, lock %s); wait for it to finish", holder, abs)
		}
		return nil, fmt.Errorf("lock %s: %w", abs, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid to run lock %s: %w", abs, err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid to run lock %s: %w", abs, err)
	}
	held[abs] = true
	return &Lock{f: f, path: abs}, nil
}

// Release unlocks, closes the lock file and forgets it in the registry.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	uerr := setLock(f, syscall.F_UNLCK)
	cerr := f.Close()
	heldMu.Lock()
	delete(held, l.path)
	heldMu.Unlock()
	if uerr != nil {
		return fmt.Errorf("unlock run lock: %w", uerr)
	}
	return cerr
}

// Held reports whether some process holds the lock on path, without taking it. When this
// process holds it, Held returns the current pid without opening the file, because closing
// a descriptor would drop the fcntl lock. Otherwise it asks the kernel with F_GETLK. A
// missing file means not held; any other failure is an error, never a guess.
func Held(path string) (pid int, isHeld bool, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, false, fmt.Errorf("resolve run lock path %s: %w", path, err)
	}
	abs = filepath.Clean(abs)
	heldMu.Lock()
	defer heldMu.Unlock()
	if held[abs] {
		return os.Getpid(), true, nil
	}
	f, err := os.OpenFile(abs, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("open run lock %s: %w", abs, err)
	}
	defer f.Close()
	fl := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &fl); err != nil {
		return 0, false, fmt.Errorf("probe run lock %s: %w", abs, err)
	}
	if fl.Type == syscall.F_UNLCK {
		return 0, false, nil
	}
	return int(fl.Pid), true, nil
}
