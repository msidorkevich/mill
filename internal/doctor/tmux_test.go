//go:build unix

package doctor

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// doctorTmuxFake writes a fake tmux whose -V runs versionCmd and whose display-message
// prints the session "work" for pane %5 only. Any other call fails.
func doctorTmuxFake(t *testing.T, versionCmd string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tmux")
	testenv.WriteExecutable(t, bin, []byte("#!/bin/sh\n"+
		"case \"$*\" in\n"+
		"-V) "+versionCmd+" ;;\n"+
		"'display-message -p -t %5 #{session_name}') echo work ;;\n"+
		"*) echo \"unexpected tmux call: $*\" >&2; exit 2 ;;\n"+
		"esac\n"))
	return bin
}

// doctorTmuxRun runs doctor in a new repository with host.kind = kind and host.tmux_bin =
// bin. bin is looked up and run for real when present; the other tools are simulated. It
// returns the report and the session name expected outside tmux.
func doctorTmuxRun(t *testing.T, kind, bin string, present bool, env map[string]string) (Report, string) {
	t.Helper()
	dir := gitRepo(t)
	p := initRepo(t, dir, "check_cmd = \"true\"\n[host]\nkind = \""+kind+"\"\ntmux_bin = \""+bin+"\"\n")
	opts := fakeEnv{tools: goodTools, getenv: env}.opts(dir)
	lookPath, output := opts.LookPath, opts.Output
	opts.LookPath = func(name string) (string, error) {
		if name != bin {
			return lookPath(name)
		}
		if !present {
			return "", exec.ErrNotFound
		}
		return bin, nil
	}
	opts.Output = func(name string, args ...string) (string, error) {
		if name != bin {
			return output(name, args...)
		}
		out, err := exec.Command(name, args...).Output()
		return string(out), err
	}
	return Run(context.Background(), opts), "mill-" + strings.ReplaceAll(filepath.Base(p.Root), ".", "-")
}

var doctorTmuxInside = map[string]string{"TMUX": "/tmp/tmux-1/default,1,0", "TMUX_PANE": "%5"}

func TestDoctorTmuxHostShowsVersionAndSession(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, kind, desc string
		env              map[string]string
		session          string // "" = the repository's detached session
	}{
		{"inside tmux", "tmux", "tmux", doctorTmuxInside, "work"},
		{"outside tmux", "tmux", "tmux", nil, ""},
		{"auto inside tmux", "auto", "auto → tmux", doctorTmuxInside, "work"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, session := doctorTmuxRun(t, c.kind, doctorTmuxFake(t, "echo 'tmux 3.4'"), true, c.env)
			if c.session != "" {
				session = c.session
			}
			got := byName(r)["host"]
			want := c.desc + " (tmux 3.4, session " + session + ")"
			if got.Status != OK || got.Detail != want || !r.OK {
				t.Fatalf("host = %+v (report ok %v), want ok %q", got, r.OK, want)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"tmux_version":"tmux 3.4"`, `"tmux_session":"` + session + `"`} {
				if !strings.Contains(string(b), field) {
					t.Errorf("JSON %s lacks %s", b, field)
				}
			}
		})
	}
}

func TestDoctorTmuxHostMissingBinaryFailsWithInstallHint(t *testing.T) {
	t.Parallel()
	r, _ := doctorTmuxRun(t, "tmux", "/missing/tmux", false, nil)
	got := byName(r)["host"]
	if got.Status != Fail || r.OK {
		t.Fatalf("host = %+v (report ok %v), want fail", got, r.OK)
	}
	for _, want := range []string{"install tmux", "brew install tmux", "package manager", `host.kind = "auto"`} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q lacks %q", got.Detail, want)
		}
	}
}

func TestDoctorTmuxHostFailsWhenItCannotReadVersionOrSession(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, versionCmd, want string
		env                    map[string]string
	}{
		{"-V fails", "echo 'tmux 3.4'; exit 1", "-V` failed", nil},
		{"-V prints nothing", "true", "printed no version", nil},
		{"unknown pane", "echo 'tmux 3.4'", "find the current tmux session", map[string]string{"TMUX": "x", "TMUX_PANE": "%9"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := doctorTmuxRun(t, "tmux", doctorTmuxFake(t, c.versionCmd), true, c.env)
			got := byName(r)["host"]
			if got.Status != Fail || r.OK || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("host = %+v (report ok %v), want fail containing %q", got, r.OK, c.want)
			}
		})
	}
}

func TestDoctorTmuxHostNotShownForAutoOutsideTmux(t *testing.T) {
	t.Parallel()
	r, _ := doctorTmuxRun(t, "auto", doctorTmuxFake(t, "echo 'tmux 3.4'"), true, nil)
	got := byName(r)["host"]
	if got.Status != OK || got.Detail != "auto → exec" || got.TmuxVersion != "" || got.TmuxSession != "" {
		t.Fatalf("host = %+v, want ok \"auto → exec\" with no tmux fields", got)
	}
}
