// Package runlock keeps two mill runs from working on one repository at the same time.
//
// The lock is an exclusive flock on a file. The kernel drops it when the process dies, so a
// killed run never leaves a stale lock behind.
package runlock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Lock is a held run lock.
type Lock struct{ f *os.File }

// Acquire takes the lock on path without blocking, creating the file, and records the
// current pid in it. When another process holds the lock the error names that process.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := "unknown pid"
		if data, rerr := os.ReadFile(path); rerr == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				holder = "pid " + s
			}
		}
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another mill run is active (%s, lock %s); wait for it to finish", holder, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid to run lock %s: %w", path, err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid to run lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	uerr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	cerr := f.Close()
	if uerr != nil {
		return fmt.Errorf("unlock run lock: %w", uerr)
	}
	return cerr
}
