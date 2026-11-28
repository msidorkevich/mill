//go:build unix

package host

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// selectEnv returns a Getenv over vars and a LookPath that finds only the named binaries,
// at /opt/bin/<name>.
func selectEnv(vars map[string]string, bins ...string) (func(string) string, func(string) (string, error)) {
	getenv := func(k string) string { return vars[k] }
	lookPath := func(file string) (string, error) {
		for _, b := range bins {
			if b == file {
				return "/opt/bin/" + file, nil
			}
		}
		return "", errors.New("not found")
	}
	return getenv, lookPath
}

func TestSelectPicksHostByKindAndEnvironment(t *testing.T) {
	t.Parallel()
	herdrEnv := map[string]string{"HERDR_ENV": "1"}
	tmuxEnv := map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}
	bothEnv := map[string]string{"HERDR_ENV": "1", "TMUX": "/tmp/tmux-1/default,1,0"}
	for _, tc := range []struct {
		name string
		kind string
		vars map[string]string
		bins []string
		host string
		desc string
		err  string
	}{
		{"auto in herdr", "auto", herdrEnv, []string{"herdr", "tmux"}, "herdr", "auto → herdr", ""},
		{"auto herdr wins over tmux", "auto", bothEnv, []string{"herdr", "tmux"}, "herdr", "auto → herdr", ""},
		{"auto in tmux", "auto", tmuxEnv, []string{"tmux"}, "tmux", "auto → tmux", ""},
		{"auto in tmux without binary", "auto", tmuxEnv, nil, "exec", "auto → exec", ""},
		{"auto outside tmux with binary", "auto", nil, []string{"tmux"}, "exec", "auto → exec", ""},
		{"tmux inside tmux", "tmux", tmuxEnv, []string{"tmux"}, "tmux", "tmux", ""},
		{"tmux outside tmux", "tmux", nil, []string{"tmux"}, "tmux", "tmux", ""},
		{"tmux without binary", "tmux", tmuxEnv, nil, "", "", "install tmux"},
		{"tmux without binary suggests auto", "tmux", nil, nil, "", "", `set host.kind = "auto" or "exec"`},
		{"unknown lists tmux", "bogus", nil, nil, "", "", `"herdr" or "tmux"`},
	} {
		getenv, lookPath := selectEnv(tc.vars, tc.bins...)
		h, desc, err := Select(tc.kind, Settings{}, getenv, lookPath)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || h.Name() != tc.host || desc != tc.desc {
			t.Errorf("%s: Select = %v %q %v, want %s %q", tc.name, h, desc, err, tc.host, tc.desc)
			continue
		}
		if tm, ok := h.(*Tmux); ok && tm.Bin != "/opt/bin/tmux" {
			t.Errorf("%s: tmux Bin = %q, want the PATH lookup", tc.name, tm.Bin)
		}
	}
}

func TestSelectPassesSettingsToHosts(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	s := Settings{TmuxBin: "my-tmux", KeepFailed: 7 * time.Second, RepoRoot: "/r/repo", Out: &out}
	getenv, lookPath := selectEnv(map[string]string{"TMUX": "x"}, "my-tmux", "herdr")
	h, _, err := Select("tmux", s, getenv, lookPath)
	if err != nil {
		t.Fatal(err)
	}
	tm := h.(*Tmux)
	if tm.Bin != "/opt/bin/my-tmux" || tm.KeepFailed != 7*time.Second || tm.RepoRoot != "/r/repo" || tm.Out != &out || tm.Getenv("TMUX") != "x" {
		t.Errorf("tmux host = %+v", tm)
	}
	if h, desc, err := Select("auto", s, getenv, lookPath); err != nil || desc != "auto → tmux" || h.(*Tmux).Bin != "/opt/bin/my-tmux" {
		t.Errorf("auto with host.tmux_bin = %v %q %v", h, desc, err)
	}
	// The default tmux binary is not on this PATH, only my-tmux.
	if _, _, err := Select("tmux", Settings{}, getenv, lookPath); err == nil {
		t.Error("tmux host selected without the tmux binary")
	}
	h, _, err = Select("herdr", s, func(k string) string { return map[string]string{"HERDR_ENV": "1"}[k] }, lookPath)
	if err != nil {
		t.Fatal(err)
	}
	if hd := h.(*Herdr); hd.KeepFailed != 7*time.Second {
		t.Errorf("herdr KeepFailed = %v, want 7s", hd.KeepFailed)
	}
}

func TestInTmuxNeedsTMUXAndTheBinary(t *testing.T) {
	t.Parallel()
	in, found := selectEnv(map[string]string{"TMUX": "x"}, "tmux")
	out, _ := selectEnv(nil)
	_, missing := selectEnv(nil)
	if !InTmux(in, found, "tmux") || InTmux(out, found, "tmux") || InTmux(in, missing, "tmux") || InTmux(in, found, "other") {
		t.Error("InTmux disagrees with TMUX and PATH")
	}
}
