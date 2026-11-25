package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

func TestParseGitVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want [3]int
	}{
		{"git version 2.50.1 (Apple Git-155)\n", [3]int{2, 50, 1}},
		{"git version 2.38.0", [3]int{2, 38, 0}},
		{"git version 2.39.5.windows.1", [3]int{2, 39, 5}},
		{"git version 3.0", [3]int{3, 0, 0}},
	}
	for _, tt := range tests {
		got, err := ParseGitVersion(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseGitVersion(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	if _, err := ParseGitVersion("hg 6.0"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestAtLeast(t *testing.T) {
	t.Parallel()
	if !atLeast([3]int{2, 38, 0}, MinGit) || !atLeast([3]int{2, 50, 1}, MinGit) || !atLeast([3]int{3, 0, 0}, MinGit) {
		t.Error("supported version rejected")
	}
	if atLeast([3]int{2, 37, 9}, MinGit) || atLeast([3]int{1, 99, 0}, MinGit) {
		t.Error("old version accepted")
	}
}

// fakeGitBin is the git path injected as GitBin. It names a tool in f.tools by base name.
const fakeGitBin = "/fake/bin/git"

// fakeEnv simulates tools on PATH. Real git is still used for repository checks.
type fakeEnv struct {
	tools  map[string]string // name → `--version` output; absent = not on PATH
	getenv map[string]string
}

func (f fakeEnv) opts(dir string) Options {
	return Options{
		Dir:    dir,
		GitBin: func() string { return fakeGitBin },
		LookPath: func(name string) (string, error) {
			if _, ok := f.tools[filepath.Base(name)]; ok {
				return "/fake/bin/" + filepath.Base(name), nil
			}
			return "", exec.ErrNotFound
		},
		Output: func(name string, args ...string) (string, error) {
			out, ok := f.tools[filepath.Base(name)]
			if !ok {
				return "", errors.New("not found")
			}
			return out, nil
		},
		Getenv: func(k string) string { return f.getenv[k] },
	}
}

func byName(r Report) map[string]Check {
	m := map[string]Check{}
	for _, c := range r.Checks {
		m[c.Name] = c
	}
	return m
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(gitx.Bin()); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command(gitx.Bin(), "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// initRepo mimics `mill init` without importing the CLI.
func initRepo(t *testing.T, dir, cfgText string) project.Paths {
	t.Helper()
	root, err := project.GitRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := project.PathsAt(root)
	if err := os.MkdirAll(p.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.GitIgnore, []byte(GitignoreText()), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), p.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return p
}

var goodTools = map[string]string{"git": "git version 2.50.1", "claude": "2.1.0 (Claude Code)"}

func TestAllGreen(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	initRepo(t, dir, `check_cmd = "go test ./..."`)
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	if !r.OK {
		t.Fatalf("report not OK: %+v", r.Checks)
	}
	for _, c := range r.Checks {
		if c.Status != OK {
			t.Errorf("%s = %s (%s), want ok", c.Name, c.Status, c.Detail)
		}
	}
}

func TestGitCheckNamesResolvedBinary(t *testing.T) {
	t.Parallel()
	opts := fakeEnv{tools: goodTools}.opts(t.TempDir())
	const bin = "/opt/git/bin/git"
	opts.GitBin = func() string { return bin }
	output := opts.Output
	var ran []string
	opts.Output = func(name string, args ...string) (string, error) {
		ran = append(ran, name)
		return output(name, args...)
	}
	c := byName(Run(context.Background(), opts))["git"]
	if c.Status != OK || !strings.Contains(c.Detail, bin) {
		t.Fatalf("git = %+v, want ok with %s in detail", c, bin)
	}
	if !slices.Contains(ran, bin) {
		t.Errorf("--version never ran on %s; ran %v", bin, ran)
	}
	if slices.Contains(ran, "git") {
		t.Errorf("--version ran on PATH git instead of the resolved binary; ran %v", ran)
	}
}

func TestMissingGitNamesResolvedBinaryAndFix(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), fakeEnv{}.opts(t.TempDir()))
	c := byName(r)["git"]
	if r.OK || c.Status != Fail || !strings.Contains(c.Detail, fakeGitBin) || !strings.Contains(c.Detail, "PATH") {
		t.Fatalf("missing git = %+v, want fail naming %s and the PATH fix", c, fakeGitBin)
	}
}

func TestOldGitFailsAndSkipsTheRest(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), fakeEnv{tools: map[string]string{"git": "git version 2.30.1"}}.opts(t.TempDir()))
	c := byName(r)
	if r.OK || c["git"].Status != Fail || !strings.Contains(c["git"].Detail, fakeGitBin) {
		t.Fatalf("old git not failed with its path: %+v", r.Checks)
	}
	for _, name := range []string{"repo", "init", "config", "database", "runner", "host"} {
		if c[name].Status != Skip {
			t.Errorf("%s = %s, want skip", name, c[name].Status)
		}
	}
}

func TestNotInitialized(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	c := byName(r)
	if r.OK || c["repo"].Status != OK || c["init"].Status != Fail || c["config"].Status != Skip {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

func TestEmptyCheckCmdWarns(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	initRepo(t, dir, config.DefaultTOML)
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	if c := byName(r)["config"]; c.Status != Warn {
		t.Fatalf("config = %+v, want warn", c)
	}
	if !r.OK {
		t.Fatal("a warning must not fail the report")
	}
}

func TestInvalidConfigFails(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	initRepo(t, dir, "max_parallel = 0")
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	c := byName(r)
	if r.OK || c["config"].Status != Fail || c["runner"].Status != Skip {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

func TestMissingClaudeFails(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	initRepo(t, dir, `check_cmd = "true"`)
	r := Run(context.Background(), fakeEnv{tools: map[string]string{"git": "git version 2.50.1"}}.opts(dir))
	if c := byName(r)["runner"]; c.Status != Fail {
		t.Fatalf("runner = %+v, want fail", c)
	}
}

func TestFakeRunnerScript(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	p := initRepo(t, dir, "check_cmd = \"true\"\n[runner]\nkind = \"fake\"\nfake_script = \"script.json\"\n")
	opts := fakeEnv{tools: map[string]string{"git": "git version 2.50.1"}}.opts(dir)

	if c := byName(Run(context.Background(), opts))["runner"]; c.Status != Fail {
		t.Fatalf("missing script: runner = %+v, want fail", c)
	}
	if err := os.WriteFile(filepath.Join(p.Root, "script.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := byName(Run(context.Background(), opts))["runner"]; c.Status != OK {
		t.Fatalf("with script: runner = %+v, want ok", c)
	}
}

func TestMissingDatabaseFails(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	p := initRepo(t, dir, `check_cmd = "true"`)
	for _, f := range []string{p.DB, p.DB + "-wal", p.DB + "-shm"} {
		os.Remove(f)
	}
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	if c := byName(r)["database"]; c.Status != Fail {
		t.Fatalf("database = %+v, want fail", c)
	}
	if _, err := os.Stat(p.DB); err == nil {
		t.Fatal("doctor created the database; it must only report")
	}
}

func TestHostCheckReportsEffectiveHost(t *testing.T) {
	t.Parallel()
	withHerdr := map[string]string{"git": "git version 2.50.1", "claude": "2.1.0", "herdr": "herdr 0.9.5"}
	cases := []struct {
		name   string
		kind   string
		tools  map[string]string
		env    map[string]string
		status Status
		detail string
	}{
		{"exec", "exec", goodTools, nil, OK, "exec"},
		{"exec inside herdr", "exec", withHerdr, map[string]string{"HERDR_ENV": "1"}, OK, "exec"},
		{"auto outside herdr", "auto", withHerdr, nil, OK, "auto → exec"},
		{"auto inside herdr without binary", "auto", goodTools, map[string]string{"HERDR_ENV": "1"}, OK, "auto → exec"},
		{"auto inside herdr", "auto", withHerdr, map[string]string{"HERDR_ENV": "1"}, OK, "auto → herdr"},
		{"herdr inside herdr", "herdr", withHerdr, map[string]string{"HERDR_ENV": "1"}, OK, "herdr"},
		{"herdr outside herdr", "herdr", withHerdr, nil, Fail, "host.kind = herdr needs mill to run inside a herdr pane"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := gitRepo(t)
			initRepo(t, dir, "check_cmd = \"true\"\n[host]\nkind = \""+c.kind+"\"\n")
			r := Run(context.Background(), fakeEnv{tools: c.tools, getenv: c.env}.opts(dir))
			got := byName(r)["host"]
			if got.Status != c.status || !strings.Contains(got.Detail, c.detail) {
				t.Fatalf("host = %+v, want %s containing %q", got, c.status, c.detail)
			}
		})
	}
}

func TestGitignoreMissingLinesWarn(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	p := initRepo(t, dir, `check_cmd = "go test ./..."`)
	if err := os.WriteFile(p.GitIgnore, []byte("runs/\nmill.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	c := byName(r)["gitignore"]
	if c.Status != Warn || !strings.Contains(c.Detail, "mill.db-*") || !strings.Contains(c.Detail, "run.lock") ||
		!strings.Contains(c.Detail, "run `mill init`") || strings.Contains(c.Detail, "runs/,") {
		t.Fatalf("gitignore check = %+v", c)
	}
	if !r.OK {
		t.Errorf("a gitignore warning must not fail the report")
	}
}

func TestGitignoreSkippedWhenNotInitialized(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t)
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	if c := byName(r)["gitignore"]; c.Status != Skip {
		t.Fatalf("gitignore check = %+v", c)
	}
}
