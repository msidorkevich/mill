package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mill/internal/budget"
	"mill/internal/config"
	"mill/internal/gitx"
	"mill/internal/host"
	"mill/internal/plan"
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

// goal approves a goal whose plan has the given tasks (id -> depends_on, in the order given).
func (e *env) goal(t *testing.T, deps ...[]string) {
	t.Helper()
	ctx := context.Background()
	var tasks []taskspec.Spec
	for i := 0; i < len(deps); i += 1 {
		id := string(rune('a' + i))
		tasks = append(tasks, taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "create a file",
			Acceptance: []string{"file exists"}, Checks: []string{}, Owns: []string{}, Size: "S", DependsOn: deps[i]})
	}
	g, err := e.p.Store.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.ApproveGoal(ctx, g.ID, plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
}

func goalImpl(file, body string) runner.Step { return impl(map[string]string{file: body}) }

func TestGoalTaskPathsAndDependencyBase(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {goalImpl("a.txt", "a\n"), goalImpl("b.txt", "b\n")},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.goal(t, nil, []string{"a"})
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("a: %+v", out)
	}
	if out := e.run(t, "g1/b"); out.Status != store.TaskVerified {
		t.Fatalf("b: %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	ba, _ := e.p.Store.GetBranch(ctx, "g1/a")
	bb, _ := e.p.Store.GetBranch(ctx, "g1/b")
	if ba.Name != "mill/g1/a" || bb.Name != "mill/g1/b" {
		t.Fatalf("branches %s, %s", ba.Name, bb.Name)
	}
	if want := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), "g1", "b"); bb.Worktree != want {
		t.Fatalf("worktree %s, want %s", bb.Worktree, want)
	}
	if _, err := os.Stat(filepath.Join(e.p.Paths.Runs, "g1", "b", "a1", "diff.patch")); err != nil {
		t.Fatal(err)
	}
	if bb.Base != "mill/g1/a" || bb.BaseSHA != ba.HeadSHA || bb.StartSHA != ba.HeadSHA {
		t.Fatalf("b branch = %+v, a head %s", bb, ba.HeadSHA)
	}
	if _, err := os.Stat(filepath.Join(bb.Worktree, "a.txt")); err != nil {
		t.Fatal("dependency file missing in worktree")
	}
}

func TestFanInMergesOtherDependenciesAndDiffsFromStart(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {goalImpl("a.txt", "a-content\n"), goalImpl("b.txt", "b-content\n"), goalImpl("c.txt", "c-content\n")},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass"), verdict("pass")},
	}, nil)
	e.goal(t, nil, nil, []string{"a", "b"})
	for _, id := range []string{"g1/a", "g1/b", "g1/c"} {
		if out := e.run(t, id); out.Status != store.TaskVerified {
			t.Fatalf("%s: %+v\n%s", id, out, e.out)
		}
	}
	ctx := context.Background()
	bb, _ := e.p.Store.GetBranch(ctx, "g1/b")
	bc, _ := e.p.Store.GetBranch(ctx, "g1/c")
	if bc.Base != "mill/g1/b" || bc.BaseSHA != bb.HeadSHA || bc.StartSHA == bc.BaseSHA || bc.StartSHA == "" {
		t.Fatalf("c branch = %+v", bc)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(bc.Worktree, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
	if parents, _ := gitx.Run(ctx, bc.Worktree, "rev-parse", bc.StartSHA+"^@"); len(strings.Fields(parents)) != 2 {
		t.Fatalf("start is not a merge commit: %q", parents)
	}
	vp := prompts(e.fake, runner.RoleVerifier)[2]
	if !strings.Contains(vp, "c-content") || strings.Contains(vp, "a-content") || strings.Contains(vp, "b-content") {
		t.Fatalf("verifier prompt has foreign changes:\n%s", vp)
	}
	patch, _ := os.ReadFile(filepath.Join(e.p.Paths.Runs, "g1", "c", "a1", "diff.patch"))
	if !strings.Contains(string(patch), "c-content") || strings.Contains(string(patch), "a-content") || strings.Contains(string(patch), "b-content") {
		t.Fatalf("diff.patch has foreign changes:\n%s", patch)
	}
}

func TestConflictingDependenciesBlockTask(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {goalImpl("same.txt", "from a\n"), goalImpl("same.txt", "from b\n")},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass")},
	}, nil)
	e.goal(t, nil, nil, []string{"a", "b"})
	e.run(t, "g1/a")
	e.run(t, "g1/b")
	out := e.run(t, "g1/c")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "g1/a") || !strings.Contains(out.Reason, "same.txt") {
		t.Fatalf("outcome = %+v", out)
	}
	task, _ := e.p.Store.GetTask(context.Background(), "g1/c")
	if task.Status != store.TaskBlocked {
		t.Fatalf("status = %s", task.Status)
	}
	if gitx.BranchExists(context.Background(), e.root, "mill/g1/c") {
		t.Fatal("conflicted branch left behind")
	}
	if _, err := os.Stat(filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), "g1", "c")); err == nil {
		t.Fatal("worktree left behind")
	}
}

func TestUnverifiedDependencyBlocksTask(t *testing.T) {
	e := setup(t, `true`, runner.Script{}, nil)
	e.goal(t, nil, []string{"a"})
	out := e.run(t, "g1/b")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "g1/a") || !strings.Contains(out.Reason, "pending") {
		t.Fatalf("outcome = %+v", out)
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("agent ran")
	}
}

func TestGoalBudgetBlocksGoalTask(t *testing.T) {
	e := setup(t, `true`, runner.Script{}, func(c *config.Config) {
		c.Budget.Goal = budget.Limits{Tokens: 1000}
	})
	e.goal(t, nil)
	ctx := context.Background()
	id, err := e.p.Store.StartAttempt(ctx, "g1/a", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.FinishAttempt(ctx, id, "ok", budget.Usage{InputTokens: 900}, "", ""); err != nil {
		t.Fatal(err)
	}
	out := e.run(t, "g1/a")
	if out.Status != store.TaskBlocked || !strings.Contains(out.Reason, "goal budget exhausted") || !strings.Contains(out.Reason, "900") {
		t.Fatalf("outcome = %+v", out)
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("agent ran")
	}
}

func TestRetryWithoutStartSHAUsesBaseSHA(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {goalImpl("x.txt", "x\n")},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "old")
	ctx := context.Background()
	// A branch recorded before start_sha existed.
	baseSHA, _ := gitx.RevParse(ctx, e.root, "main")
	wt := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), "old")
	if err := gitx.AddWorktree(ctx, e.root, wt, "mill/t/old", baseSHA); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.SaveBranch(ctx, store.Branch{TaskID: "old", Name: "mill/t/old", Base: "main", BaseSHA: baseSHA, HeadSHA: baseSHA, Worktree: wt}); err != nil {
		t.Fatal(err)
	}
	out := e.run(t, "old")
	if out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if vp := prompts(e.fake, runner.RoleVerifier)[0]; !strings.Contains(vp, "+x") {
		t.Fatalf("verifier prompt:\n%s", vp)
	}
}

func TestRequestsCarryTaskID(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "ident")
	e.run(t, "ident")
	calls := e.fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	for _, c := range calls {
		if c.TaskID != "ident" {
			t.Errorf("%s request TaskID = %q, want ident", c.Role, c.TaskID)
		}
	}
}

func TestParallelGoalTasksShareOnePipeline(t *testing.T) {
	step := func(file string) []runner.Step {
		return []runner.Step{{DelayMS: 50, Files: map[string]string{file: "x\n"},
			Output: json.RawMessage(`{"summary":"s","files_changed":[],"notes":""}`)}}
	}
	pass := []runner.Step{verdict("pass")}
	e := setup(t, `true`, runner.Script{}, nil)
	e.fake = runner.NewFake(runner.FakeScript{Tasks: map[string]runner.Script{
		"g1/a": {runner.RoleImplementer: step("a.txt"), runner.RoleVerifier: pass},
		"g1/b": {runner.RoleImplementer: step("b.txt"), runner.RoleVerifier: pass},
	}})
	e.p.Runner = e.fake
	e.goal(t, []string{}, []string{})
	ids := []string{"g1/a", "g1/b"}
	outs := make([]Outcome, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = e.p.Run(context.Background(), id)
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || outs[i].Status != store.TaskVerified {
			t.Fatalf("%s: %+v, %v\n%s", ids[i], outs[i], errs[i], e.out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(e.out.String()), "\n") {
		if !strings.HasPrefix(line, "g1/") {
			t.Errorf("progress line is not whole: %q", line)
		}
	}
}

func TestReattachCommitsInterruptedWork(t *testing.T) {
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a": "1"})},
		runner.RoleVerifier:    {verdict("fail", "nope")},
	}, func(c *config.Config) { c.MaxAttempts = 1 })
	e.task(t, "crash")
	e.run(t, "crash")
	ctx := context.Background()
	br, _ := e.p.Store.GetBranch(ctx, "crash")
	if err := os.WriteFile(filepath.Join(br.Worktree, "partial.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.SetTaskStatus(ctx, "crash", store.TaskPending, "", ""); err != nil {
		t.Fatal(err)
	}
	e.fake = runner.NewFake(runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"b": "2"})},
		runner.RoleVerifier:    {verdict("pass")},
	})
	e.p.Runner = e.fake
	if out := e.run(t, "crash"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	log, _ := gitx.Run(ctx, br.Worktree, "log", "--format=%B")
	if !strings.Contains(log, "mill: crash interrupted attempt") || !strings.Contains(log, "Mill-Task: crash") {
		t.Fatalf("no interrupted-attempt commit:\n%s", log)
	}
	if out, _ := gitx.Run(ctx, br.Worktree, "show", "HEAD~1:partial.txt"); strings.TrimSpace(out) != "wip" {
		t.Fatalf("partial.txt not committed before the next attempt: %q", out)
	}
}

// startingRunner reports a started process like the claude runner, and records what the
// store holds for the attempt while the run is in flight.
type startingRunner struct {
	*runner.Fake
	t    *testing.T
	s    *store.Store
	mu   sync.Mutex
	seen []store.Attempt
}

func (r *startingRunner) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	req.OnStart(runner.Started{PID: 4321, Command: "claude", RawPath: "/raw/" + string(req.Role) + ".jsonl"})
	running, err := r.s.RunningAttempts(context.Background())
	if err != nil {
		r.t.Error(err)
	}
	r.mu.Lock()
	r.seen = append(r.seen, running...)
	r.mu.Unlock()
	return r.Fake.Run(ctx, req)
}

func TestAgentAttemptRecordsProcessWhileRunning(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "x\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "t1")
	sr := &startingRunner{Fake: e.fake, t: t, s: e.p.Store}
	e.p.Runner = sr
	e.run(t, "t1")
	if len(sr.seen) == 0 {
		t.Fatal("no running attempt seen")
	}
	for _, a := range sr.seen {
		if a.PID != 4321 || a.Command != "claude" || a.LogPath != "/raw/"+a.Role+".jsonl" {
			t.Fatalf("running attempt = %+v", a)
		}
	}
}
