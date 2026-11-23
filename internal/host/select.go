//go:build unix

package host

import (
	"errors"
	"fmt"
)

// InHerdr reports whether mill runs inside a herdr pane (HERDR_ENV=1) with herdr on PATH.
func InHerdr(getenv func(string) string, lookPath func(string) (string, error)) bool {
	if getenv("HERDR_ENV") != "1" {
		return false
	}
	_, err := lookPath("herdr")
	return err == nil
}

// Select returns the host for host.kind ("auto", "exec" or "herdr") and a description of
// the choice, such as "auto → herdr".
func Select(kind string, getenv func(string) string, lookPath func(string) (string, error)) (Host, string, error) {
	herdr := func() Host {
		h := NewHerdr()
		h.Workspace = getenv("HERDR_WORKSPACE_ID")
		if p, err := lookPath("herdr"); err == nil {
			h.Bin = p
		}
		return h
	}
	switch kind {
	case "exec":
		return NewExec(), "exec", nil
	case "auto":
		if InHerdr(getenv, lookPath) {
			return herdr(), "auto → herdr", nil
		}
		return NewExec(), "auto → exec", nil
	case "herdr":
		if !InHerdr(getenv, lookPath) {
			return nil, "", errors.New(`host.kind = herdr needs mill to run inside a herdr pane (HERDR_ENV=1) with herdr on PATH; otherwise set host.kind = "auto" or "exec"`)
		}
		return herdr(), "herdr", nil
	}
	return nil, "", fmt.Errorf("unknown host.kind %q: set it to \"auto\", \"exec\" or \"herdr\"", kind)
}
