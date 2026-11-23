package tui

import (
	"context"
	"errors"
	"os/exec"
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
