package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// openModel returns a model over s with fake env and recorders for herdr and the shell.
func openModel(t *testing.T, s Store, env map[string]string, herdr bool) (m *Model, herdrArgs *[][]string, shell *[][2]string) {
	t.Helper()
	herdrArgs, shell = &[][]string{}, &[][2]string{}
	m = New(Options{
		Store:  s,
		Paths:  project.PathsAt(t.TempDir()),
		Runner: NewFakeRunner(),
		Now:    func() time.Time { return clock },
		Getenv: func(k string) string { return env[k] },
		LookPath: func(string) (string, error) {
			if herdr {
				return "/bin/herdr", nil
			}
			return "", errors.New("not found")
		},
		Herdr: func(_ context.Context, args []string) (string, string, int, error) {
			*herdrArgs = append(*herdrArgs, args)
			return "", "", 0, nil
		},
		ShellCmd: func(sh, dir string) *exec.Cmd {
			*shell = append(*shell, [2]string{sh, dir})
			c := exec.Command("true")
			c.Dir = dir
			return c
		},
	})
	return m, herdrArgs, shell
}

func saveBranch(t *testing.T, s *store.Store, worktree string) {
	t.Helper()
	if err := s.SaveBranch(context.Background(), store.Branch{TaskID: "g1/b", Name: "mill/g1/b", Base: "master", Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenInHerdrSplitsPane(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, herdr, shell := openModel(t, s, map[string]string{"HERDR_ENV": "1"}, true)
	if cmd := m.openWorktree("g1/b"); cmd != nil {
		t.Errorf("want no tea command inside herdr")
	}
	want := [][]string{{"pane", "split", "--current", "--direction", "right", "--cwd", "/wt/b"}}
	if !reflect.DeepEqual(*herdr, want) || len(*shell) != 0 {
		t.Errorf("herdr calls = %v, shell = %v", *herdr, *shell)
	}
}

func TestOpenInHerdrFailureShowsStderr(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, _, _ := openModel(t, s, map[string]string{"HERDR_ENV": "1"}, true)
	m.opts.Herdr = func(context.Context, []string) (string, string, int, error) {
		return "", "no such pane\n", 1, nil
	}
	m.openWorktree("g1/b")
	if out := m.cmds.render(100, 10); !strings.Contains(out, "no such pane") {
		t.Errorf("panel lacks herdr stderr:\n%s", out)
	}
}

func TestOpenOutsideHerdrRunsShellInWorktree(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ env, want string }{
		"shell set":   {"/bin/zsh", "/bin/zsh"},
		"shell unset": {"", "/bin/sh"},
	} {
		t.Run(name, func(t *testing.T) {
			s := seed(t)
			saveBranch(t, s, "/wt/b")
			m, herdr, shell := openModel(t, s, map[string]string{"SHELL": tc.env}, false)
			var cmd tea.Cmd = m.openWorktree("g1/b")
			if cmd == nil {
				t.Fatal("want a tea command")
			}
			want := [][2]string{{tc.want, "/wt/b"}}
			if !reflect.DeepEqual(*shell, want) || len(*herdr) != 0 {
				t.Errorf("shell = %v, herdr = %v", *shell, *herdr)
			}
		})
	}
}

func TestOpenWithoutWorktreeReportsAndRunsNothing(t *testing.T) {
	t.Parallel()
	s := seed(t)
	for name, wt := range map[string]string{"no branch": "", "cleaned": "-"} {
		t.Run(name, func(t *testing.T) {
			if wt == "-" {
				saveBranch(t, s, "")
			}
			m, herdr, shell := openModel(t, s, map[string]string{"HERDR_ENV": "1"}, true)
			if cmd := m.openWorktree("g1/b"); cmd != nil {
				t.Error("want no command")
			}
			if len(*herdr) != 0 || len(*shell) != 0 {
				t.Errorf("ran something: %v %v", *herdr, *shell)
			}
			if out := m.cmds.render(100, 10); !strings.Contains(out, "no worktree") {
				t.Errorf("panel lacks error:\n%s", out)
			}
		})
	}
}

// tmuxRecorder records the tmux calls of a model and answers them with the given result.
func tmuxRecorder(m *Model, stderr string, code int, err error) *[][]string {
	calls := &[][]string{}
	m.opts.Tmux = func(_ context.Context, args []string) (string, string, int, error) {
		*calls = append(*calls, args)
		return "", stderr, code, err
	}
	return calls
}

func TestOpenWithTmuxHostRunsNewWindowInWorktree(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, herdr, shell := openModel(t, s, map[string]string{"TMUX": "/tmp/tmux,1,0"}, false)
	m.opts.HostKind = "tmux"
	calls := tmuxRecorder(m, "", 0, nil)
	if cmd := m.openWorktree("g1/b"); cmd != nil {
		t.Errorf("want no tea command for tmux")
	}
	want := [][]string{{"new-window", "-c", "/wt/b"}}
	if !reflect.DeepEqual(*calls, want) || len(*herdr) != 0 || len(*shell) != 0 {
		t.Errorf("tmux = %v, herdr = %v, shell = %v", *calls, *herdr, *shell)
	}
	if out := m.cmds.render(100, 10); !strings.Contains(out, "opened /wt/b in a tmux window") {
		t.Errorf("panel lacks success note:\n%s", out)
	}
	if out := m.cmds.render(100, 10); strings.Contains(out, "attach") {
		t.Errorf("attach hint inside tmux:\n%s", out)
	}
}

func TestOpenWithTmuxFailureShowsError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stderr string
		code   int
		err    error
		want   string
	}{
		"exit status": {"no server running on /tmp/tmux-501/default\n", 1, nil, "tmux exited 1: no server running"},
		"cannot run":  {"", -1, errors.New("exec: \"tmux\": executable file not found"), "run tmux: exec: \"tmux\""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := seed(t)
			saveBranch(t, s, "/wt/b")
			m, _, shell := openModel(t, s, map[string]string{}, false)
			m.opts.HostKind = "tmux"
			tmuxRecorder(m, tc.stderr, tc.code, tc.err)
			m.openWorktree("g1/b")
			if out := m.cmds.render(100, 10); !strings.Contains(out, tc.want) || strings.Contains(out, "opened") {
				t.Errorf("panel lacks error %q:\n%s", tc.want, out)
			}
			if len(*shell) != 0 {
				t.Errorf("ran a shell after a tmux failure: %v", *shell)
			}
		})
	}
}

func TestOpenWithTmuxHostInsideHerdrUsesHerdr(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, herdr, shell := openModel(t, s, map[string]string{"HERDR_ENV": "1"}, true)
	m.opts.HostKind = "tmux"
	calls := tmuxRecorder(m, "", 0, nil)
	m.openWorktree("g1/b")
	want := [][]string{{"pane", "split", "--current", "--direction", "right", "--cwd", "/wt/b"}}
	if !reflect.DeepEqual(*herdr, want) || len(*calls) != 0 || len(*shell) != 0 {
		t.Errorf("herdr = %v, tmux = %v, shell = %v", *herdr, *calls, *shell)
	}
}

func TestOpenWithExecHostRunsShellNotTmux(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, herdr, shell := openModel(t, s, map[string]string{"SHELL": "/bin/zsh"}, false)
	m.opts.HostKind = "exec"
	calls := tmuxRecorder(m, "", 0, nil)
	if cmd := m.openWorktree("g1/b"); cmd == nil {
		t.Fatal("want a tea command")
	}
	want := [][2]string{{"/bin/zsh", "/wt/b"}}
	if !reflect.DeepEqual(*shell, want) || len(*herdr) != 0 || len(*calls) != 0 {
		t.Errorf("shell = %v, herdr = %v, tmux = %v", *shell, *herdr, *calls)
	}
}

func TestOpenWithTmuxHostOutsideTmuxSaysToAttach(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	m, _, shell := openModel(t, s, map[string]string{}, false)
	m.opts.HostKind = "tmux"
	calls := tmuxRecorder(m, "", 0, nil)
	m.openWorktree("g1/b")
	want := [][]string{{"new-window", "-c", "/wt/b"}}
	if !reflect.DeepEqual(*calls, want) || len(*shell) != 0 {
		t.Errorf("tmux = %v, shell = %v", *calls, *shell)
	}
	out := m.cmds.render(100, 10)
	for _, w := range []string{"opened /wt/b in a tmux window", "not inside tmux", "`tmux attach`"} {
		if !strings.Contains(out, w) {
			t.Errorf("panel lacks %q:\n%s", w, out)
		}
	}
}

func TestOpenWithTmuxHostExecsConfiguredBinary(t *testing.T) {
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "my-tmux"), filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New(Options{
		Store:    s,
		Paths:    project.PathsAt(t.TempDir()),
		Runner:   NewFakeRunner(),
		Getenv:   func(string) string { return "" },
		HostKind: "tmux",
		TmuxBin:  bin,
	})
	m.openWorktree("g1/b")
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the configured tmux did not run: %v\n%s", err, m.cmds.render(0, 10))
	}
	if want := "new-window\n-c\n/wt/b\n"; string(got) != want {
		t.Errorf("tmux args = %q, want %q", got, want)
	}
}

func TestOpenAfterHostSelectionErrorShowsItAndRunsShell(t *testing.T) {
	t.Parallel()
	s := seed(t)
	saveBranch(t, s, "/wt/b")
	var shells []string
	m := New(Options{
		Store:   s,
		Paths:   project.PathsAt(t.TempDir()),
		Runner:  NewFakeRunner(),
		Getenv:  func(string) string { return "" },
		HostErr: errors.New("host.kind = tmux needs the tmux binary, which was not found: install tmux (or set host.tmux_bin), or set host.kind to exec"),
		ShellCmd: func(_, dir string) *exec.Cmd {
			shells = append(shells, dir)
			return exec.Command("true")
		},
	})
	// The whole error must be readable at a normal width, including what to do next.
	out := strings.Join(strings.Fields(m.cmds.render(80, 10)), " ")
	for _, w := range []string{"o opens a shell", "which was not found", "set host.kind to exec"} {
		if !strings.Contains(out, w) {
			t.Errorf("panel lacks %q:\n%s", w, out)
		}
	}
	if cmd := m.openWorktree("g1/b"); cmd == nil || !reflect.DeepEqual(shells, []string{"/wt/b"}) {
		t.Errorf("shells = %v", shells)
	}
}
