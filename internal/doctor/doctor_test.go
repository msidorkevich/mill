package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mill/internal/config"
	"mill/internal/project"
	"mill/internal/store"
)

func TestParseGitVersion(t *testing.T) {
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
	if !atLeast([3]int{2, 38, 0}, MinGit) || !atLeast([3]int{2, 50, 1}, MinGit) || !atLeast([3]int{3, 0, 0}, MinGit) {
		t.Error("supported version rejected")
	}
	if atLeast([3]int{2, 37, 9}, MinGit) || atLeast([3]int{1, 99, 0}, MinGit) {
		t.Error("old version accepted")
	}
}

// fakeEnv simulates tools on PATH. Real git is still used for repository checks.
type fakeEnv struct {
	tools  map[string]string // name → `--version` output; absent = not on PATH
	getenv map[string]string
}

func (f fakeEnv) opts(dir string) Options {
	return Options{
		Dir: dir,
		LookPath: func(name string) (string, error) {
			if _, ok := f.tools[name]; ok {
				return "/fake/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		Output: func(name string, args ...string) (string, error) {
			out, ok := f.tools[name]
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
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
	s, err := store.Open(context.Background(), p.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return p
}

var goodTools = map[string]string{"git": "git version 2.50.1", "claude": "2.1.0 (Claude Code)"}

func TestAllGreen(t *testing.T) {
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

func TestOldGitFailsAndSkipsTheRest(t *testing.T) {
	r := Run(context.Background(), fakeEnv{tools: map[string]string{"git": "git version 2.30.1"}}.opts(t.TempDir()))
	c := byName(r)
	if r.OK || c["git"].Status != Fail {
		t.Fatalf("old git not failed: %+v", r.Checks)
	}
	for _, name := range []string{"repo", "init", "config", "database", "runner", "host"} {
		if c[name].Status != Skip {
			t.Errorf("%s = %s, want skip", name, c[name].Status)
		}
	}
}

func TestNotInitialized(t *testing.T) {
	dir := gitRepo(t)
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	c := byName(r)
	if r.OK || c["repo"].Status != OK || c["init"].Status != Fail || c["config"].Status != Skip {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

func TestEmptyCheckCmdWarns(t *testing.T) {
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
	dir := gitRepo(t)
	initRepo(t, dir, "max_parallel = 0")
	r := Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
	c := byName(r)
	if r.OK || c["config"].Status != Fail || c["runner"].Status != Skip {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

func TestMissingClaudeFails(t *testing.T) {
	dir := gitRepo(t)
	initRepo(t, dir, `check_cmd = "true"`)
	r := Run(context.Background(), fakeEnv{tools: map[string]string{"git": "git version 2.50.1"}}.opts(dir))
	if c := byName(r)["runner"]; c.Status != Fail {
		t.Fatalf("runner = %+v, want fail", c)
	}
}

func TestFakeRunnerScript(t *testing.T) {
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

func TestHostAutoInsideHerdrWarns(t *testing.T) {
	dir := gitRepo(t)
	initRepo(t, dir, `check_cmd = "true"`)
	tools := map[string]string{"git": "git version 2.50.1", "claude": "2.1.0", "herdr": "herdr 0.9.5"}
	r := Run(context.Background(), fakeEnv{tools: tools, getenv: map[string]string{"HERDR_PANE_ID": "p1"}}.opts(dir))
	if c := byName(r)["host"]; c.Status != Warn {
		t.Fatalf("host = %+v, want warn", c)
	}
	r = Run(context.Background(), fakeEnv{tools: tools}.opts(dir))
	if c := byName(r)["host"]; c.Status != OK {
		t.Fatalf("outside herdr: host = %+v, want ok", c)
	}
}
