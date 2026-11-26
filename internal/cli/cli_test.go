package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/doctor"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, dir string, d *doctor.Options, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), Env{Stdout: &out, Stderr: &errb, Dir: dir, Doctor: d}, args)
	return result{code, out.String(), errb.String()}
}

// runEnv runs mill in dir with env's injected fields (Getenv, LookPath, ...); Stdout, Stderr
// and Dir are set here.
func runEnv(t *testing.T, dir string, env Env, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr, env.Dir = &out, &errb, dir
	code := Run(context.Background(), env, args)
	return result{code, out.String(), errb.String()}
}

// fakeGetenv returns an Env.Getenv that reads vars only, so a command never sees the
// process environment.
func fakeGetenv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command(gitx.Bin(), "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// fakeGitBin is the git binary tools() tells doctor mill runs.
const fakeGitBin = "/fake/git"

// tools simulates git and the agent CLIs, so doctor tests never depend on the machine's
// git, claude or herdr.
func tools(have map[string]string) *doctor.Options {
	// git is asked for by the path GitBin returns, the other tools by name.
	key := func(name string) string {
		if name == fakeGitBin {
			return "git"
		}
		return name
	}
	return &doctor.Options{
		GitBin: func() string { return fakeGitBin },
		LookPath: func(name string) (string, error) {
			if _, ok := have[key(name)]; ok {
				return "/fake/" + key(name), nil
			}
			return "", exec.ErrNotFound
		},
		Output: func(name string, _ ...string) (string, error) {
			if out, ok := have[key(name)]; ok {
				return out, nil
			}
			return "", errors.New("missing")
		},
		Getenv: func(string) string { return "" },
	}
}

var machine = map[string]string{"git": "git version 2.50.1", "claude": "2.1.0 (Claude Code)", "nice": ""}

func TestUsageAndUnknownCommand(t *testing.T) {
	t.Parallel()
	if r := run(t, t.TempDir(), nil); r.code != ExitUsage || !strings.Contains(r.stderr, "Usage") {
		t.Fatalf("no args: %+v", r)
	}
	if r := run(t, t.TempDir(), nil, "frobnicate"); r.code != ExitUsage || !strings.Contains(r.stderr, `unknown command "frobnicate"`) {
		t.Fatalf("unknown: %+v", r)
	}
	if r := run(t, t.TempDir(), nil, "help"); r.code != ExitOK || !strings.Contains(r.stdout, "doctor") {
		t.Fatalf("help: %+v", r)
	}
	if r := run(t, t.TempDir(), nil, "--help"); r.code != ExitOK {
		t.Fatalf("--help: %+v", r)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "version")
	if r.code != ExitOK || r.stdout != "mill dev\n" {
		t.Fatalf("version: %+v", r)
	}
	if r := run(t, t.TempDir(), nil, "version", "extra"); r.code != ExitUsage {
		t.Fatalf("version extra: %+v", r)
	}
	if r := run(t, t.TempDir(), nil, "version", "--bogus"); r.code != ExitUsage {
		t.Fatalf("bad flag: %+v", r)
	}
}

func TestInitOutsideGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	// git reads GIT_CEILING_DIRECTORIES from the process environment while project discovery
	// runs it, and discovery takes no environment of its own, so this test stays serial: the
	// ceiling keeps git from finding a repository above the temp dir.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	r := run(t, dir, nil, "init")
	if r.code != ExitError || !strings.Contains(r.stderr, "not inside a git repository") {
		t.Fatalf("init outside repo: %+v", r)
	}
}

func TestInitCreatesLayoutFromSubdirectory(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	sub := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := run(t, sub, nil, "init")
	if r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	p := project.PathsAt(root)
	for _, f := range []string{p.Config, p.GitIgnore, p.DB} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s not created: %v", f, err)
		}
	}
	if got, _ := os.ReadFile(p.GitIgnore); !strings.Contains(string(got), "run.lock") {
		t.Errorf(".mill/.gitignore does not ignore run.lock: %q", got)
	}
	if fi, err := os.Stat(p.Runs); err != nil || !fi.IsDir() {
		t.Errorf("runs/ not created: %v", err)
	}
	if got, _ := os.ReadFile(p.Config); string(got) != config.RenderDefaultTOML(gitBranch(t, root)) {
		t.Error("config.toml is not the default template")
	}

	// The init event is in the log.
	s, err := store.Open(context.Background(), p.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	evs, err := s.Events(context.Background(), 0, 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != "init" {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}

func TestInitGitignoreKeepsStateOutOfGit(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	p := project.PathsAt(root)
	if err := os.WriteFile(filepath.Join(p.Runs, "log.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(gitx.Bin(), "-C", root, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	want := map[string]bool{"??": true, ".mill/.gitignore": true, ".mill/config.toml": true}
	for _, f := range got {
		if !want[f] {
			t.Errorf("git sees %q; only config.toml and .gitignore should be visible:\n%s", f, out)
		}
	}
}

func gitBranch(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command(gitx.Bin(), "-C", root, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestInitWritesCurrentBranchAsBase(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"master", "main", "trunk"} {
		root := gitRepo(t) // no commits: unborn branch
		if out, err := exec.Command(gitx.Bin(), "-C", root, "symbolic-ref", "HEAD", "refs/heads/"+name).CombinedOutput(); err != nil {
			t.Fatalf("symbolic-ref: %v: %s", err, out)
		}
		r := run(t, root, nil, "init")
		if r.code != ExitOK || !strings.Contains(r.stdout, name) {
			t.Fatalf("init on %s: %+v", name, r)
		}
		cfg, err := config.Load(project.PathsAt(root).Config)
		if err != nil || cfg.BaseBranch != name {
			t.Fatalf("base_branch on %s = %q, %v", name, cfg.BaseBranch, err)
		}
		if got, _ := os.ReadFile(project.PathsAt(root).Config); !strings.Contains(string(got), "# The gate") {
			t.Errorf("template comments lost: %q", got)
		}
	}
}

func TestInitDetachedHeadKeepsMainAndAsksToCheck(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	git := func(args ...string) {
		t.Helper()
		a := append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)
		if out, err := exec.Command(gitx.Bin(), a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("commit", "-q", "--allow-empty", "-m", "x")
	git("checkout", "-q", "--detach")
	r := run(t, root, nil, "init")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Check base_branch") {
		t.Fatalf("init detached: %+v", r)
	}
	cfg, err := config.Load(project.PathsAt(root).Config)
	if err != nil || cfg.BaseBranch != "main" {
		t.Fatalf("base_branch = %q, %v", cfg.BaseBranch, err)
	}
}

func TestInitExistingConfigKeepsBranchAndReportsIt(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	p := project.PathsAt(root)
	if err := os.MkdirAll(p.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := "base_branch = \"develop\"\n"
	if err := os.WriteFile(p.Config, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "init")
	if r.code != ExitOK || !strings.Contains(r.stdout, "develop") {
		t.Fatalf("init: %+v", r)
	}
	if got, _ := os.ReadFile(p.Config); string(got) != custom {
		t.Fatalf("config changed: %q", got)
	}
}

func TestInitIsIdempotentAndKeepsConfig(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("first init: %+v", r)
	}
	p := project.PathsAt(root)
	custom := `check_cmd = "make check"` + "\n"
	if err := os.WriteFile(p.Config, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "init")
	if r.code != ExitOK || !strings.Contains(r.stdout, "kept") {
		t.Fatalf("second init: %+v", r)
	}
	if got, _ := os.ReadFile(p.Config); string(got) != custom {
		t.Fatalf("init overwrote config: %q", got)
	}
}

func TestInitRefusesInvalidExistingConfig(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	p := project.PathsAt(root)
	if err := os.MkdirAll(p.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte("max_parallel = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "init")
	if r.code != ExitError || !strings.Contains(r.stderr, "invalid") {
		t.Fatalf("init with bad config: %+v", r)
	}
}

func TestDoctorAfterInit(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	// Default config: check_cmd empty → warning, but the report passes.
	r := run(t, root, tools(machine), "doctor")
	if r.code != ExitOK {
		t.Fatalf("doctor: %+v", r)
	}
	if !strings.Contains(r.stdout, "warn  config") || !strings.Contains(r.stdout, "ok    database") {
		t.Fatalf("doctor output:\n%s", r.stdout)
	}
	if want := "git 2.50.1 (>= 2.38.0) at " + fakeGitBin; !strings.Contains(r.stdout, want) {
		t.Fatalf("doctor output lacks %q:\n%s", want, r.stdout)
	}
}

func TestDoctorFailsWithExitCode3(t *testing.T) {
	t.Parallel()
	root := gitRepo(t) // not initialized
	r := run(t, root, tools(machine), "doctor")
	if r.code != ExitCheckFailed || !strings.Contains(r.stdout, "FAIL  init") {
		t.Fatalf("doctor: %+v", r)
	}
}

func TestDoctorJSON(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	run(t, root, nil, "init")
	r := run(t, root, tools(machine), "doctor", "--json")
	if r.code != ExitOK {
		t.Fatalf("doctor --json: %+v", r)
	}
	var doc struct {
		Schema string         `json:"schema"`
		OK     bool           `json:"ok"`
		Checks []doctor.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, r.stdout)
	}
	if doc.Schema != "mill/doctor@1" || !doc.OK || len(doc.Checks) == 0 {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestInitAppendsMissingGitignoreLines(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	p := project.PathsAt(root)
	if err := os.MkdirAll(p.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "# mine\nmy-own\nruns/\nmill.db\nmill.db-*"
	if err := os.WriteFile(p.GitIgnore, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "init")
	if r.code != ExitOK || !strings.Contains(r.stdout, ".gitignore   updated (added: run.lock)") {
		t.Fatalf("init: %+v", r)
	}
	got, _ := os.ReadFile(p.GitIgnore)
	if string(got) != old+"\nrun.lock\n" {
		t.Errorf(".gitignore = %q", got)
	}
	r = run(t, root, nil, "init")
	if r.code != ExitOK || !strings.Contains(r.stdout, ".gitignore   kept") {
		t.Fatalf("second init: %+v", r)
	}
	again, _ := os.ReadFile(p.GitIgnore)
	if string(again) != string(got) {
		t.Errorf("up-to-date .gitignore changed: %q", again)
	}
}
