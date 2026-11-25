package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// macShim is Apple's xcrun shim, which finds the developer directory on every call before it
// starts the real git.
const macShim = "/usr/bin/git"

// resolver finds the git binary to run. Its fields are injected so tests need no real git.
type resolver struct {
	lookPath  func(string) (string, error)
	xcrunFind func() (string, error)
	goos      string
	isExec    func(string) bool
}

// resolve returns the git on PATH, or the binary behind it when that is the macOS shim. Any
// failure to see past the shim keeps the PATH result.
func (r resolver) resolve() string {
	path, err := r.lookPath("git")
	if err != nil {
		return "git" // exec then reports the usual not-found error
	}
	if r.goos != "darwin" || filepath.Clean(path) != macShim {
		return path
	}
	found, err := r.xcrunFind()
	if err != nil || !filepath.IsAbs(found) || !r.isExec(found) {
		return path
	}
	return found
}

func xcrunFindGit() (string, error) {
	out, err := exec.Command("/usr/bin/xcrun", "--find", "git").Output()
	return strings.TrimSpace(string(out)), err
}

// isExecutable reports whether path is an existing regular file with an execute bit.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

var (
	binOnce sync.Once
	binPath string
)

// Bin returns the git binary mill runs. It is resolved on first use, once per process, and
// is safe for concurrent use.
func Bin() string {
	binOnce.Do(func() {
		binPath = resolver{
			lookPath:  exec.LookPath,
			xcrunFind: xcrunFindGit,
			goos:      runtime.GOOS,
			isExec:    isExecutable,
		}.resolve()
	})
	return binPath
}
