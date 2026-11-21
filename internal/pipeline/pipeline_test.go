package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mill/internal/budget"
	"mill/internal/config"
	"mill/internal/gitx"
	"mill/internal/host"
	"mill/internal/project"
	"mill/internal/runner"
	"mill/internal/store"
	"mill/internal/taskspec"
)

type env struct {
	p    *Pipeline
	fake *runner.Fake
	out  *bytes.Buffer
	root string
}

// setup creates a repo with one commit on main, an initialized .mill/, and a pipeline
// using the fake runner. checkCmd is the project gate.
func setup(t *testing.T, checkCmd string, script runner.Script, tweak func(*config.Config)) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	root, _ := filepath.EvalSymlinks(dir)
	if _, err := gitx.Run(ctx, root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".mill/runs/\n.mill/mill.db*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "initial"); err != nil {
		t.Fatal(err)
	}
	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := config.Default()
	cfg.CheckCmd = checkCmd
	cfg.WorktreeRoot = t.TempDir()
	cfg.Runner.Kind = "fake"
	cfg.Runner.FakeScript = "unused"
	cfg.GateTimeout = config.Duration{Duration: 10 * time.Second}
	cfg.Budget.Task = budget.Limits{} // unlimited unless a test sets it
	if tweak != nil {
		tweak(&cfg)
	}
	fake := runner.NewFake(script)
	out := &bytes.Buffer{}
	return &env{
		p:    &Pipeline{Store: s, Runner: fake, Host: host.NewExec(), Cfg: cfg, Paths: paths, Home: t.TempDir(), Out: out},
		fake: fake, out: out, root: root,
	}
}

func (e *env) task(t *testing.T, id string, checks ...string) {
	t.Helper()
	spec := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "create hello.txt",
		Acceptance: []string{"hello.txt says hello"}, Checks: checks, Owns: []string{}, Size: "S"}
	if err := e.p.Store.CreateTask(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}

func impl(files map[string]string, usage ...budget.Usage) runner.Step {
	return runner.Step{Output: json.RawMessage(`{"summary":"wrote files","files_changed":[],"notes":""}`), Files: files, Usage: usage}
}

func verdict(v string, findings ...string) runner.Step {
	type f struct {
		File  string `json:"file"`
		Line  int    `json:"line"`
		Issue string `json:"issue"`
	}
	fs := []f{}
	for _, issue := range findings {
		fs = append(fs, f{"hello.txt", 1, issue})
	}
	b, _ := json.Marshal(map[string]any{"verdict": v, "summary": "review of the change", "findings": fs})
	return runner.Step{Output: b}
}

func (e *env) run(t *testing.T, id string) Outcome {
	t.Helper()
	out, err := e.p.Run(context.Background(), id)
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, e.out)
	}
	return out
}

func prompts(f *runner.Fake, role runner.Role) []string {
	var ps []string
	for _, c := range f.Calls() {
		if c.Role == role {
			ps = append(ps, c.Prompt)
		}
	}
	return ps
}

func TestHappyPath(t *testing.T) {
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"}, budget.Usage{InputTokens: 100, CostUSD: 0.1})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "hello")
	out := e.run(t, "hello")

	if out.Status != store.TaskVerified || out.Attempts != 1 || out.HeadSHA == "" {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	task, _ := e.p.Store.GetTask(ctx, "hello")
	if task.Status != store.TaskVerified {
		t.Fatalf("task status = %s", task.Status)
	}
	br, err := e.p.Store.GetBranch(ctx, "hello")
	if err != nil || br.Name != "mill/t/hello" || br.HeadSHA != out.HeadSHA || br.HeadSHA == br.BaseSHA {
		t.Fatalf("branch = %+v, %v", br, err)
	}
	// The commit is on the task branch, carries a trailer, and main is untouched.
	msg, _ := gitx.Run(ctx, br.Worktree, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "Mill-Task: hello") || !strings.Contains(msg, "wrote files") {
		t.Fatalf("commit message = %q", msg)
	}
	if _, err := os.Stat(filepath.Join(e.root, "hello.txt")); err == nil {
		t.Fatal("change leaked into the main checkout")
	}
	mainSHA, _ := gitx.RevParse(ctx, e.root, "main")
	if mainSHA != br.BaseSHA {
		t.Fatal("main moved")
	}
	// The worktree lives outside the repository.
	if strings.HasPrefix(br.Worktree, e.root) {
		t.Fatalf("worktree %s is inside the repo", br.Worktree)
	}
	// The verifier got the diff and the gate result, not the implementer transcript.
	vp := prompts(e.fake, runner.RoleVerifier)
	if len(vp) != 1 || !strings.Contains(vp[0], "+hello") || !strings.Contains(vp[0], "grep -q hello hello.txt`: passed") {
		t.Fatalf("verifier prompt:\n%s", vp)
	}
	// Spend is recorded per attempt.
	if out.Usage.InputTokens != 100 || out.Usage.CostUSD != 0.1 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	atts, _ := e.p.Store.Attempts(ctx, "hello")
	if len(atts) != 2 || atts[0].Role != "implementer" || atts[1].Role != "verifier" || atts[0].Status != "ok" {
		t.Fatalf("attempts = %+v", atts)
	}
	// Implementer and verifier got their role's tools and the .mill/ directory protected.
	for _, c := range e.fake.Calls() {
		if c.Dir != br.Worktree || len(c.ProtectedPaths) != 1 || c.ProtectedPaths[0] != e.p.Paths.MillDir {
			t.Fatalf("request = %+v", c)
		}
		if c.Role == runner.RoleVerifier && strings.Contains(strings.Join(c.Tools, ","), "Edit") {
			t.Fatal("verifier allowed to edit")
		}
	}
}

func TestGateFailThenRetryPasses(t *testing.T) {
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {
			impl(map[string]string{"hello.txt": "goodbye\n"}),
			impl(map[string]string{"hello.txt": "hello\n"}),
		},
		runner.RoleVerifier: {verdict("pass")},
	}, nil)
	e.task(t, "retry")
	out := e.run(t, "retry")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if len(ip) != 2 || !strings.Contains(ip[1], "[gate] `grep -q hello hello.txt` failed (exit 1)") {
		t.Fatalf("second implementer prompt lacks the gate finding:\n%s", ip[1])
	}
	if len(prompts(e.fake, runner.RoleVerifier)) != 1 {
		t.Fatal("verifier ran on a change that failed the gate")
	}
	if !strings.Contains(e.out.String(), "gate      FAIL") {
		t.Fatalf("log:\n%s", e.out)
	}
}

func TestVerifierFailThenPass(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "helo\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("fail", "typo: helo"), verdict("pass")},
	}, nil)
	e.task(t, "vfail")
	out := e.run(t, "vfail")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if ip := prompts(e.fake, runner.RoleImplementer); !strings.Contains(ip[1], "[verifier] hello.txt:1: typo: helo") {
		t.Fatalf("finding not passed on:\n%s", ip[1])
	}
}

func TestMaxAttemptsBlocks(t *testing.T) {
	e := setup(t, `false`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}), impl(map[string]string{"a": "2"})},
	}, func(c *config.Config) { c.MaxAttempts = 2 })
	e.task(t, "stuck")
	out := e.run(t, "stuck")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "not verified after 2 attempt(s)") {
		t.Fatalf("outcome = %+v", out)
	}
	task, _ := e.p.Store.GetTask(context.Background(), "stuck")
	if task.Status != store.TaskBlocked {
		t.Fatalf("status = %s", task.Status)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "stuck")
	if len(f) != 1 || f[0].Source != "gate" {
		t.Fatalf("last findings = %+v", f)
	}
}

func TestEmptyGateFails(t *testing.T) {
	e := setup(t, "", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
	}, func(c *config.Config) { c.MaxAttempts = 1 })
	e.task(t, "nogate")
	out := e.run(t, "nogate")
	if out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v", out)
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "nogate")
	if len(f) != 1 || !strings.Contains(f[0].Issue, "no checks configured") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestTaskChecksRunInGate(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}), impl(map[string]string{"b": "1"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "checks", "test -f b")
	out := e.run(t, "checks")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
}

func TestNoChangesIsAFailedAttempt(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(nil), impl(map[string]string{"a": "1"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "lazy")
	out := e.run(t, "lazy")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	if ip := prompts(e.fake, runner.RoleImplementer); !strings.Contains(ip[1], "no changes were made") {
		t.Fatalf("prompt:\n%s", ip[1])
	}
}

func TestImplementerErrorIsAFindingAndChangesAreKept(t *testing.T) {
	e := setup(t, "test -f a && test -f b", runner.Script{
		runner.RoleImplementer: {
			{Error: "model overloaded", Files: nil},
			impl(map[string]string{"a": "1", "b": "1"}),
		},
		runner.RoleVerifier: {verdict("pass")},
	}, nil)
	e.task(t, "flaky")
	out := e.run(t, "flaky")
	if out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if ip := prompts(e.fake, runner.RoleImplementer); !strings.Contains(ip[1], "the implementer run failed: model overloaded") {
		t.Fatalf("prompt:\n%s", ip[1])
	}
}

func TestAttemptTokenLimitBlocks(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"},
			budget.Usage{InputTokens: 600}, budget.Usage{InputTokens: 600}, budget.Usage{InputTokens: 600})},
	}, func(c *config.Config) {
		c.Budget.Attempt = budget.Limits{Tokens: 1000}
		c.Budget.Size = nil
	})
	e.task(t, "pricey")
	out := e.run(t, "pricey")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "budget exceeded in attempt 1") || !strings.Contains(out.Reason, "token limit") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if len(e.fake.Calls()) != 1 {
		t.Fatal("pipeline retried after a budget stop (BG5 says no automatic retry)")
	}
	atts, _ := e.p.Store.Attempts(context.Background(), "pricey")
	if atts[0].Status != "budget_exceeded" || atts[0].Usage.InputTokens != 1200 {
		t.Fatalf("attempt = %+v", atts[0])
	}
}

func TestRunnerBudgetStatusBlocks(t *testing.T) {
	e := setup(t, "true", runner.Script{runner.RoleImplementer: {impl(nil)}}, nil)
	e.p.Runner = budgetRunner{}
	e.task(t, "usd")
	out := e.run(t, "usd")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "max-budget-usd") {
		t.Fatalf("outcome = %+v", out)
	}
}

type budgetRunner struct{}

func (budgetRunner) Name() string { return "budget" }
func (budgetRunner) Run(context.Context, runner.Request) (runner.Result, error) {
	return runner.Result{Status: runner.StatusBudgetExceeded, Error: "claude stopped at --max-budget-usd", Usage: budget.Usage{CostUSD: 1}}, nil
}

func TestTaskBudgetBlocksBeforeAttempt(t *testing.T) {
	e := setup(t, "false", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}, budget.Usage{CostUSD: 0.4})},
	}, func(c *config.Config) {
		c.Budget.Attempt = budget.Limits{USD: 0.5}
		c.Budget.Size = nil
		c.Budget.Task = budget.Limits{USD: 0.8}
	})
	e.task(t, "capped")
	out := e.run(t, "capped")
	// Attempt 1 spends 0.4 (under its 0.5 limit); attempt 2 may need 0.5 but only 0.4 is left.
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "task budget cannot cover another attempt") {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if len(e.fake.Calls()) != 1 {
		t.Fatalf("calls = %d, want 1", len(e.fake.Calls()))
	}
}

func TestRetryContinuesOnSameBranchWithFindings(t *testing.T) {
	e := setup(t, "grep -q hello hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "nope\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.MaxAttempts = 1 })
	e.task(t, "again")
	if out := e.run(t, "again"); out.Status != store.TaskBlocked {
		t.Fatalf("first run = %+v", out)
	}
	ctx := context.Background()
	br1, _ := e.p.Store.GetBranch(ctx, "again")

	// Simulate a lost worktree directory: the retry reattaches the branch.
	if err := os.RemoveAll(br1.Worktree); err != nil {
		t.Fatal(err)
	}
	out := e.run(t, "again")
	if out.Status != store.TaskVerified {
		t.Fatalf("retry = %+v\n%s", out, e.out)
	}
	br2, _ := e.p.Store.GetBranch(ctx, "again")
	if br2.BaseSHA != br1.BaseSHA || br2.Name != br1.Name {
		t.Fatalf("retry changed the base: %+v vs %+v", br2, br1)
	}
	ip := prompts(e.fake, runner.RoleImplementer)
	if !strings.Contains(ip[1], "Attempt 1 of 1") || !strings.Contains(ip[1], "[gate]") {
		t.Fatalf("retry prompt lacks the earlier findings:\n%s", ip[1])
	}
	atts, _ := e.p.Store.Attempts(ctx, "again")
	if atts[len(atts)-1].N != 2 {
		t.Fatalf("attempt numbering did not continue: %+v", atts)
	}
}

func TestExistingUnknownBranchIsRefused(t *testing.T) {
	e := setup(t, "true", runner.Script{}, nil)
	if _, err := gitx.Run(context.Background(), e.root, "branch", "mill/t/clash"); err != nil {
		t.Fatal(err)
	}
	e.task(t, "clash")
	out, err := e.p.Run(context.Background(), "clash")
	if err == nil || out.Status != store.TaskFailed || !strings.Contains(out.Reason, "already exists") {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
}

func TestInterruptBlocks(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"}, budget.Usage{InputTokens: 1}, budget.Usage{InputTokens: 1})},
	}, nil)
	e.task(t, "ctrlc")
	ctx, cancel := context.WithCancel(context.Background())
	e.p.Runner = cancelRunner{inner: e.fake, cancel: cancel}
	out, err := e.p.Run(ctx, "ctrlc")
	if err != nil || out.Status != store.TaskBlocked || out.Reason != "interrupted" {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
}

// cancelRunner cancels the whole run (like Ctrl-C) on the first usage report.
type cancelRunner struct {
	inner  runner.Runner
	cancel context.CancelFunc
}

func (c cancelRunner) Name() string { return "cancel" }
func (c cancelRunner) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	orig := req.OnUsage
	req.OnUsage = func(d budget.Usage) { orig(d); c.cancel() }
	return c.inner.Run(ctx, req)
}
