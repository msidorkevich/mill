package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestHelpListsTUI(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "help")
	if r.code != ExitOK || !strings.Contains(r.stdout, "tui      interactive terminal UI") {
		t.Fatalf("help: %+v", r)
	}
}

func TestTUIHelpFlagExitsZero(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "tui", "-h")
	if r.code != ExitOK || !strings.Contains(r.stderr, "mill tui") {
		t.Fatalf("tui -h: %+v", r)
	}
}

func TestTUIOutsideProjectFails(t *testing.T) {
	t.Parallel()
	r := run(t, gitRepo(t), nil, "tui")
	if r.code != ExitError || !strings.Contains(r.stderr, "mill tui:") {
		t.Fatalf("tui outside an initialized repo: %+v", r)
	}
}

func TestTUIHostOptionsFollowTheConfiguredHost(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind    string
		env     map[string]string
		found   bool // the binaries are on PATH
		want    string
		wantErr string
	}{
		"exec":                {kind: "exec", found: true, want: "exec"},
		"tmux":                {kind: "tmux", found: true, want: "tmux"},
		"auto inside tmux":    {kind: "auto", env: map[string]string{"TMUX": "x"}, found: true, want: "tmux"},
		"tmux binary missing": {kind: "tmux", wantErr: "host.kind = tmux needs the tmux binary"},
		"herdr outside herdr": {kind: "herdr", found: true, wantErr: "host.kind = herdr needs mill to run inside a herdr pane"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := Env{Getenv: fakeGetenv(tc.env), LookPath: func(bin string) (string, error) {
				if !tc.found {
					return "", errors.New("not found")
				}
				return "/bin/" + bin, nil
			}}
			opts := tuiHostOptions(env, tc.kind, "my-tmux", t.TempDir())
			if opts.HostKind != tc.want || opts.TmuxBin != "my-tmux" {
				t.Errorf("HostKind = %q, TmuxBin = %q; want %q, my-tmux", opts.HostKind, opts.TmuxBin, tc.want)
			}
			switch {
			case tc.wantErr == "" && opts.HostErr != nil:
				t.Errorf("HostErr = %v", opts.HostErr)
			case tc.wantErr != "" && (opts.HostErr == nil || !strings.Contains(opts.HostErr.Error(), tc.wantErr)):
				t.Errorf("HostErr = %v, want %q", opts.HostErr, tc.wantErr)
			}
		})
	}
}
