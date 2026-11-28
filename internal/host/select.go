//go:build unix

package host

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Settings configures the hosts Select builds, from the project config.
type Settings struct {
	TmuxBin    string        // tmux executable (host.tmux_bin), looked up on PATH; "" = "tmux"
	KeepFailed time.Duration // how long a failed worker's tab or window stays open (host.keep_failed)
	RepoRoot   string        // the repository root; names and roots the tmux session outside tmux
	Out        io.Writer     // receives the tmux attach hint; nil = os.Stderr
}

// InHerdr reports whether mill runs inside a herdr pane (HERDR_ENV=1) with herdr on PATH.
func InHerdr(getenv func(string) string, lookPath func(string) (string, error)) bool {
	if getenv("HERDR_ENV") != "1" {
		return false
	}
	_, err := lookPath("herdr")
	return err == nil
}

// InTmux reports whether mill runs inside tmux ($TMUX set) with the tmux binary bin found.
func InTmux(getenv func(string) string, lookPath func(string) (string, error), bin string) bool {
	if getenv("TMUX") == "" {
		return false
	}
	_, err := lookPath(bin)
	return err == nil
}

// Select returns the host for host.kind ("auto", "exec", "herdr" or "tmux") and a
// description of the choice, such as "auto → herdr".
func Select(kind string, s Settings, getenv func(string) string, lookPath func(string) (string, error)) (Host, string, error) {
	tmuxBin := s.TmuxBin
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	herdr := func() Host {
		h := NewHerdr()
		h.Workspace = getenv("HERDR_WORKSPACE_ID")
		if p, err := lookPath("herdr"); err == nil {
			h.Bin = p
		}
		h.KeepFailed = s.KeepFailed
		return h
	}
	tmux := func(bin string) Host {
		t := NewTmux()
		t.Bin = bin
		t.RepoRoot = s.RepoRoot
		t.KeepFailed = s.KeepFailed
		t.Getenv = getenv
		if s.Out != nil {
			t.Out = s.Out
		}
		return t
	}
	switch kind {
	case "exec":
		return NewExec(), "exec", nil
	case "auto":
		if InHerdr(getenv, lookPath) {
			return herdr(), "auto → herdr", nil
		}
		if InTmux(getenv, lookPath, tmuxBin) {
			p, err := lookPath(tmuxBin)
			if err != nil {
				return nil, "", fmt.Errorf("auto host: look up tmux %q: %w", tmuxBin, err)
			}
			return tmux(p), "auto → tmux", nil
		}
		return NewExec(), "auto → exec", nil
	case "herdr":
		if !InHerdr(getenv, lookPath) {
			return nil, "", errors.New(`host.kind = herdr needs mill to run inside a herdr pane (HERDR_ENV=1) with herdr on PATH; otherwise set host.kind = "auto" or "exec"`)
		}
		return herdr(), "herdr", nil
	case "tmux":
		p, err := lookPath(tmuxBin)
		if err != nil {
			return nil, "", fmt.Errorf(`host.kind = tmux needs the tmux binary %q, which was not found: install tmux (or set host.tmux_bin), or set host.kind = "auto" or "exec"`, tmuxBin)
		}
		return tmux(p), "tmux", nil
	}
	return nil, "", fmt.Errorf("unknown host.kind %q: set it to \"auto\", \"exec\", \"herdr\" or \"tmux\"", kind)
}
