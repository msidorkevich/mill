// Package host decides where worker processes run (HO1).
//
// A host only starts, waits for and kills processes. It never interprets their output:
// results are always read from files, so the choice of host affects visibility, not
// behaviour. The exec host runs plain subprocesses; the herdr host runs each worker in its
// own herdr tab and the tmux host in its own detached tmux window, both through the same
// wrapper protocol, and both are optional (HO3–HO4). Select picks one from host.kind.
package host

import (
	"context"
	"os"
	"strings"
)

// ProcSpec describes a process to start.
type ProcSpec struct {
	Name       string   // label, e.g. "T-12 implementer" (pane title for visual hosts)
	Argv       []string // program and arguments; Argv[0] is resolved on PATH
	Dir        string   // working directory
	Env        []string // the complete environment; nil means an EMPTY environment, never inherited
	StdinPath  string   // file fed to stdin; "" = no stdin
	StdoutPath string   // file receiving stdout; "" = discarded
	StderrPath string   // file receiving stderr; "" = discarded
	LogDir     string   // where visual hosts keep wrapper scripts; "" = StdoutPath's directory, else a temp dir
}

// Exit describes how a process ended.
type Exit struct {
	Code     int  // exit code; -1 when killed by a signal
	Signaled bool // true when the process was killed by a signal
}

// Handle is a started process.
type Handle interface {
	PID() int
	// Wait blocks until the process ends. Call it exactly once.
	Wait() (Exit, error)
	// Kill stops the process and everything it started.
	Kill() error
}

// Host starts processes. Cancelling ctx kills the process and its children.
type Host interface {
	Name() string
	Start(ctx context.Context, spec ProcSpec) (Handle, error)
}

// DefaultEnvAllow lists the variables workers inherit by default (SE3). Everything else,
// such as API keys in the operator's shell, stays out unless the allowlist names it.
var DefaultEnvAllow = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "LANG", "LC_ALL", "LC_CTYPE"}

// AllowEnv returns the entries of environ ("KEY=value") whose key is in allow, in the order
// they appear in environ.
func AllowEnv(environ []string, allow []string) []string {
	set := make(map[string]bool, len(allow))
	for _, k := range allow {
		set[k] = true
	}
	out := []string{}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && set[k] {
			out = append(out, kv)
		}
	}
	return out
}

// WorkerEnv is AllowEnv over the current process environment with DefaultEnvAllow plus
// extra keys.
func WorkerEnv(extra ...string) []string {
	return AllowEnv(os.Environ(), append(append([]string{}, DefaultEnvAllow...), extra...))
}
