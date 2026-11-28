package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/testenv"
)

// useHostKind sets host.kind in the repo's config. Commands run without HERDR_* (TestMain
// clears them and tests inject their own Env.Getenv), so mill looks like it runs outside herdr.
func useHostKind(t *testing.T, root, kind string) {
	t.Helper()
	p := project.PathsAt(root)
	b, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, append(b, []byte("[host]\nkind = \""+kind+"\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRunFailsWhenHerdrHostIsUnavailable(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitError || !strings.Contains(r.stderr, "mill task run: host.kind = herdr needs mill to run inside a herdr pane") ||
		!strings.Contains(r.stderr, `"auto" or "exec"`) {
		t.Fatalf("task run: %+v", r)
	}
	if strings.Contains(r.stdout, "implementer") {
		t.Fatalf("work started despite the host error:\n%s", r.stdout)
	}
}

func TestTaskRunHostFlagOverridesConfig(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "task", "run", "--file", tf, "--host", "exec")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run --host exec: %+v", r)
	}
}

func TestRunFailsWhenHerdrHostIsUnavailableAndHostFlagOverrides(t *testing.T) {
	t.Parallel()
	root, _ := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "run", "g")
	if r.code != ExitError || !strings.Contains(r.stderr, "mill run: host.kind = herdr needs mill to run inside a herdr pane") {
		t.Fatalf("run: %+v", r)
	}
	// With --host exec the host is accepted; the run then fails on the missing goal.
	r = run(t, root, nil, "run", "g", "--host", "exec")
	if strings.Contains(r.stderr, "herdr") {
		t.Fatalf("run --host exec still complains about herdr: %+v", r)
	}
}

func TestInvalidHostFlagIsUsageError(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	if r := run(t, root, nil, "task", "run", "--file", tf, "--host", "bogus"); r.code != ExitUsage || !strings.Contains(r.stderr, "--host must be") {
		t.Fatalf("task run: %+v", r)
	}
	if r := run(t, root, nil, "run", "g", "--host", "bogus"); r.code != ExitUsage || !strings.Contains(r.stderr, "--host must be") {
		t.Fatalf("run: %+v", r)
	}
}

func TestTaskRunInsideHerdrUsesHerdrFromPath(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	lookPath, log := fakeHerdr(t)
	env := Env{Getenv: fakeGetenv(map[string]string{"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w8"}), LookPath: lookPath}
	r := runEnv(t, root, env, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the auto host never called the fake herdr: %v", err)
	}
	calls := string(b)
	if !strings.Contains(calls, "tab create --workspace w8 --label mill ") || !strings.Contains(calls, "tab close w8:t2") {
		t.Fatalf("herdr calls:\n%s", calls)
	}
}

// fakeTmuxScript is a fake tmux for the cli tests. It logs every call (one per line) to
// tmux.log next to itself, answers display-message with the session "work", keeps the
// sessions it creates in sessions, and runs the shell command of new-window (its last
// argument) in the background, printing window ids @1, @2, ...
const fakeTmuxScript = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/tmux.log"
case "$1" in
new-window)
  n=$(cat "$dir/counter" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$dir/counter"
  for a; do cmd=$a; done
  sh -c "$cmd" >> "$dir/window.out" 2>&1 < /dev/null &
  echo "@$n" ;;
kill-window) ;;
list-windows) ;;
display-message) echo work ;;
has-session) grep -qx -- "${3#=}" "$dir/sessions" 2>/dev/null || { echo "can't find session: ${3#=}" >&2; exit 1; } ;;
new-session) echo "$4" >> "$dir/sessions" ;;
*) echo "unexpected tmux call: $*" >&2; exit 2 ;;
esac
`

// fakeTmuxPath writes the fake tmux under the name bin and returns a LookPath that finds it
// (and herdr through herdrLookPath when not nil) and never a real tmux, plus the call log.
func fakeTmuxPath(t *testing.T, bin string, herdrLookPath func(string) (string, error)) (lookPath func(string) (string, error), log string) {
	t.Helper()
	dir := t.TempDir()
	testenv.WriteExecutable(t, filepath.Join(dir, bin), []byte(fakeTmuxScript))
	lookPath = func(file string) (string, error) {
		switch {
		case file == bin:
			return filepath.Join(dir, bin), nil
		case file == "tmux":
			return "", exec.ErrNotFound
		case herdrLookPath != nil:
			return herdrLookPath(file)
		}
		return exec.LookPath(file)
	}
	return lookPath, filepath.Join(dir, "tmux.log")
}

// noTmuxPath is a LookPath on which tmux is missing.
func noTmuxPath(file string) (string, error) {
	if file == "tmux" {
		return "", exec.ErrNotFound
	}
	return exec.LookPath(file)
}

// tmuxCalls returns the fake tmux's logged calls; "" when it was never called.
func tmuxCalls(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// insideTmux is the environment of a mill started inside tmux.
var insideTmux = map[string]string{"TMUX": "/tmp/tmux-1/default,1,0", "TMUX_PANE": "%5"}

func TestTaskRunHostFlagTmuxSelectsTmuxOutsideTmux(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	lookPath, log := fakeTmuxPath(t, "tmux", nil)
	r := runEnv(t, root, Env{Getenv: fakeGetenv(nil), LookPath: lookPath}, "task", "run", "--file", tf, "--host", "tmux")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run --host tmux: %+v", r)
	}
	calls := tmuxCalls(t, log)
	session := host.TmuxSessionName(root)
	if !strings.Contains(calls, "new-session -d -s "+session+" -c "+root) || !strings.Contains(calls, "new-window ") ||
		!strings.Contains(calls, "-t ="+session+":") || !strings.Contains(calls, "kill-window -t @1") {
		t.Fatalf("tmux calls:\n%s", calls)
	}
	// The attach hint for the detached session goes to stderr.
	if !strings.Contains(r.stderr, session) {
		t.Fatalf("stderr lacks the attach hint for %s:\n%s", session, r.stderr)
	}
}

func TestTaskRunHostKindTmuxUsesTmuxBinInsideTmux(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	p := project.PathsAt(root)
	b, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, append(b, []byte("[host]\nkind = \"tmux\"\ntmux_bin = \"my-tmux\"\nkeep_failed = \"0s\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	lookPath, log := fakeTmuxPath(t, "my-tmux", nil)
	r := runEnv(t, root, Env{Getenv: fakeGetenv(insideTmux), LookPath: lookPath}, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	calls := tmuxCalls(t, log)
	if !strings.Contains(calls, "-t =work:") || strings.Contains(calls, "new-session") {
		t.Fatalf("tmux calls inside tmux, want windows in the current session:\n%s", calls)
	}
}

func TestTaskRunAutoHostInsideTmuxSelectsTmux(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	lookPath, log := fakeTmuxPath(t, "tmux", nil)
	r := runEnv(t, root, Env{Getenv: fakeGetenv(insideTmux), LookPath: lookPath}, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	if calls := tmuxCalls(t, log); !strings.Contains(calls, "new-window ") {
		t.Fatalf("the auto host never opened a tmux window:\n%s", calls)
	}
}

func TestTaskRunAutoHostOutsideTmuxSelectsExecWithTmuxOnPath(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	lookPath, log := fakeTmuxPath(t, "tmux", nil)
	r := runEnv(t, root, Env{Getenv: fakeGetenv(nil), LookPath: lookPath}, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	if calls := tmuxCalls(t, log); calls != "" {
		t.Fatalf("the auto host called tmux outside tmux:\n%s", calls)
	}
}

func TestTaskRunAutoHostPrefersHerdrOverTmux(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	herdrPath, herdrLog := fakeHerdr(t)
	lookPath, log := fakeTmuxPath(t, "tmux", herdrPath)
	vars := map[string]string{"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w8"}
	for k, v := range insideTmux {
		vars[k] = v
	}
	r := runEnv(t, root, Env{Getenv: fakeGetenv(vars), LookPath: lookPath}, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	if _, err := os.Stat(herdrLog); err != nil {
		t.Fatalf("the auto host never called herdr: %v", err)
	}
	if calls := tmuxCalls(t, log); calls != "" {
		t.Fatalf("the auto host called tmux inside herdr:\n%s", calls)
	}
}

func TestTmuxHostWithoutTmuxFailsSayingToInstallIt(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	env := Env{Getenv: fakeGetenv(insideTmux), LookPath: noTmuxPath}
	r := runEnv(t, root, env, "task", "run", "--file", tf, "--host", "tmux")
	if r.code != ExitError || !strings.Contains(r.stderr, "install tmux") || !strings.Contains(r.stderr, `"auto" or "exec"`) {
		t.Fatalf("task run --host tmux: %+v", r)
	}
	if strings.Contains(r.stdout, "implementer") {
		t.Fatalf("work started despite the host error:\n%s", r.stdout)
	}
	useHostKind(t, root, "tmux")
	if r := runEnv(t, root, env, "run", "g"); r.code != ExitError || !strings.Contains(r.stderr, "mill run: host.kind = tmux needs the tmux binary") {
		t.Fatalf("run: %+v", r)
	}
}

func TestHostFlagHelpAndUsageListTmux(t *testing.T) {
	t.Parallel()
	root, tf := taskRepo(t, passScript, 3)
	r := run(t, root, nil, "task", "run", "--file", tf, "--host", "bogus")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "auto, exec, herdr or tmux") {
		t.Fatalf("task run --host bogus: %+v", r)
	}
	if r := run(t, root, nil, "run", "--help"); !strings.Contains(r.stderr, "auto, exec, herdr or tmux") {
		t.Fatalf("run --help: %+v", r)
	}
	if r := run(t, root, nil, "run", "a", "b"); r.code != ExitUsage || !strings.Contains(r.stderr, "--host auto|exec|herdr|tmux") {
		t.Fatalf("run a b: %+v", r)
	}
}
