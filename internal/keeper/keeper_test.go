package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const (
	taskID   = "g1/a"
	original = "a\nb\nc\n"
	taskSide = "a\nb task\nc\n"
	mainSide = "a\nb main\nc\n"
	resolved = "a\nb main task\nc\n"
)

type env struct {
	o    Options
	fake *runner.Fake
	out  *bytes.Buffer
	root string
	bare string
	wt   string
	base string // the commit the task started from
	head string // the task's head before the pass
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeCommit(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// setup creates a repo with a bare remote and a goal with one verified task g1/a, whose
// branch changes the second line of shared.txt. checkCmd is the gate.
func setup(t *testing.T, checkCmd string, script runner.Script) *env {
	t.Helper()
	return setupWith(t, checkCmd, script)
}

// setupWith is setup with more tasks in the goal's plan; only g1/a gets a branch.
func setupWith(t *testing.T, checkCmd string, script runner.Script, more ...taskspec.Spec) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".mill/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".gitignore")
	base := writeCommit(t, root, "shared.txt", original, "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pl := plan.Plan{Goal: "keep", Tasks: []taskspec.Spec{{ID: "a", Title: "Task a", Kind: "implement", Spec: "change b",
		Acceptance: []string{"b says task"}, Owns: []string{}, Size: "S"}}}
	pl.Tasks = append(pl.Tasks, more...)
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "keep", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "a")
	name := "mill/" + taskID
	git(t, root, "worktree", "add", "-q", "-b", name, wt, base)
	head := writeCommit(t, wt, "shared.txt", taskSide, "work on a")
	if err := s.SaveBranch(ctx, store.Branch{TaskID: taskID, Name: name, Base: "main", BaseSHA: base, StartSHA: base,
		HeadSHA: head, GateSHA: head, Worktree: wt}); err != nil {
		t.Fatal(err)
	}
	if aid, err := s.StartAttempt(ctx, taskID, 1, "implementer"); err != nil {
		t.Fatal(err)
	} else if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "task-verdict", g.ID, taskID, map[string]any{"attempt": 1, "head": head,
		"verdict": map[string]any{"verdict": "pass", "summary": "fine", "findings": []any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, taskID, store.TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.CheckCmd = checkCmd
	cfg.WorktreeRoot = t.TempDir()
	cfg.Runner.Kind = "fake"
	cfg.Runner.FakeScript = "unused"
	cfg.GateTimeout = config.Duration{Duration: 10 * time.Second}
	cfg.Budget.Task = budget.Limits{}
	cfg.GateCheckBase = false
	fake := runner.NewFake(script)
	out := &bytes.Buffer{}
	h := host.NewExec()
	p := &pipeline.Pipeline{Store: s, Runner: fake, Host: h, Cfg: cfg, Paths: paths, Home: t.TempDir(), Out: out}
	return &env{
		o:    Options{Store: s, Cfg: cfg, Paths: paths, Host: h, Pipeline: p, Out: out},
		fake: fake, out: out, root: root, bare: bare, wt: wt, base: base, head: head,
	}
}

// moveMain commits content as shared.txt on main and pushes it to the remote.
func (e *env) moveMain(t *testing.T, name, content string) string {
	t.Helper()
	sha := writeCommit(t, e.root, name, content, "main moves "+name)
	git(t, e.root, "push", "-q", "origin", "main")
	return sha
}

// recordResolution teaches rerere how to resolve the task's conflict with main, then puts the
// branch back.
func (e *env) recordResolution(t *testing.T, onto string) {
	t.Helper()
	ctx := context.Background()
	st, err := gitx.RebaseStart(ctx, e.wt, onto, e.base, true)
	if err != nil || st.Done {
		t.Fatalf("rebase = %+v, %v; want a stop", st, err)
	}
	if err := os.WriteFile(filepath.Join(e.wt, "shared.txt"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err = gitx.RebaseContinue(ctx, e.wt, st.Files, true); err != nil || !st.Done {
		t.Fatalf("continue = %+v, %v", st, err)
	}
	git(t, e.wt, "reset", "-q", "--hard", e.head)
}

func (e *env) pass(t *testing.T) Result {
	t.Helper()
	res, err := Pass(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Pass: %v\nlog:\n%s", err, e.out)
	}
	return res
}

func (e *env) roleCalls(role runner.Role) int {
	n := 0
	for _, c := range e.fake.Calls() {
		if c.Role == role {
			n++
		}
	}
	return n
}

// keeperGates counts the gate runs the keeper recorded for the task.
func (e *env) keeperGates(t *testing.T) int {
	t.Helper()
	evs, err := e.o.Store.TaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "task-gate" && strings.Contains(string(ev.Data), `"keeper":true`) {
			n++
		}
	}
	return n
}

func (e *env) restackedBy(t *testing.T) []string {
	t.Helper()
	evs, err := e.o.Store.TaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	var by []string
	for _, ev := range evs {
		if ev.Kind != "task-restacked" {
			continue
		}
		var d map[string]string
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		by = append(by, d["resolved_by"])
	}
	return by
}

func resolverStep(files map[string]string) runner.Step {
	return runner.Step{Output: json.RawMessage(`{"summary":"kept both","resolved":["shared.txt"]}`), Files: files,
		Usage: []budget.Usage{{InputTokens: 50, CostUSD: 0.03}}}
}

func verdictStep(v string) runner.Step {
	b, _ := json.Marshal(map[string]any{"verdict": v, "summary": "review of the change", "findings": []any{}})
	return runner.Step{Output: b}
}

func implStep(files map[string]string) runner.Step {
	return runner.Step{Output: json.RawMessage(`{"summary":"fixed the gate","files_changed":[],"notes":""}`), Files: files}
}

// assertUnchanged checks the branch is back at the old head, with no rebase in progress and
// a clean worktree.
func (e *env) assertUnchanged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if head := git(t, e.wt, "rev-parse", "HEAD"); head != e.head {
		t.Errorf("head = %s, want the old head %s", head, e.head)
	}
	if br, _ := e.o.Store.GetBranch(ctx, taskID); br.HeadSHA != e.head || br.StartSHA != e.base {
		t.Errorf("branch row = %+v, want head %s start %s", br, e.head, e.base)
	}
	if in, err := rebaseInProgress(ctx, e.wt); err != nil || in {
		t.Errorf("rebase in progress = %v, %v", in, err)
	}
	if dirty, err := gitx.HasChanges(ctx, e.wt); err != nil || dirty {
		t.Errorf("worktree dirty = %v, %v", dirty, err)
	}
}

func TestBranchWhoseBaseMovedWithoutConflictIsLeftAlone(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	e.moveMain(t, "other.txt", "other\n")
	res := e.pass(t)
	if len(res.Rebased) != 0 || len(res.Blocked) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if head := git(t, e.wt, "rev-parse", "HEAD"); head != e.head {
		t.Fatalf("head = %s, want unchanged %s", head, e.head)
	}
	if br, _ := e.o.Store.GetBranch(context.Background(), taskID); br.HeadSHA != e.head {
		t.Fatalf("recorded head = %s", br.HeadSHA)
	}
	if len(e.fake.Calls()) != 0 || len(e.restackedBy(t)) != 0 {
		t.Fatalf("calls = %d, restacks = %v", len(e.fake.Calls()), e.restackedBy(t))
	}
}

func TestConflictWithRecordedResolutionIsRebasedByRerere(t *testing.T) {
	e := setup(t, "grep -q 'main task' shared.txt", runner.Script{})
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)
	gatesBefore := e.keeperGates(t)

	res := e.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].ResolvedBy != ResolvedByRerere || len(res.ResolvedRerere) != 1 ||
		len(res.ResolvedAgent) != 0 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("agent calls = %d, want none (no resolver, no verifier)", n)
	}
	if e.keeperGates(t) != gatesBefore+1 {
		t.Fatalf("keeper gate runs = %d, want 1", e.keeperGates(t))
	}
	head := git(t, e.wt, "rev-parse", "HEAD")
	br, _ := e.o.Store.GetBranch(context.Background(), taskID)
	if br.HeadSHA != head || br.GateSHA != head || br.StartSHA != tip || br.BaseSHA != tip || head == e.head {
		t.Fatalf("branch = %+v, worktree head %s", br, head)
	}
	if got := git(t, e.wt, "show", "HEAD:shared.txt"); got+"\n" != resolved {
		t.Fatalf("shared.txt = %q", got)
	}
	if by := e.restackedBy(t); len(by) != 1 || by[0] != "rerere" {
		t.Fatalf("restacked events = %v", by)
	}
}

func TestConflictWithoutResolutionIsFixedByResolverAgent(t *testing.T) {
	e := setup(t, "grep -q 'main task' shared.txt", runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"shared.txt": resolved})},
		runner.RoleVerifier: {verdictStep("pass")},
	})
	tip := e.moveMain(t, "shared.txt", mainSide)

	res := e.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].ResolvedBy != ResolvedByAgent || len(res.ResolvedAgent) != 1 ||
		len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if e.roleCalls(runner.RoleResolver) != 1 || e.roleCalls(runner.RoleVerifier) != 1 || e.keeperGates(t) != 1 {
		t.Fatalf("resolver %d, verifier %d, gates %d", e.roleCalls(runner.RoleResolver), e.roleCalls(runner.RoleVerifier), e.keeperGates(t))
	}
	ctx := context.Background()
	head := git(t, e.wt, "rev-parse", "HEAD")
	br, _ := e.o.Store.GetBranch(ctx, taskID)
	if br.HeadSHA != head || br.GateSHA != head || br.StartSHA != tip || head == e.head || res.Rebased[0].NewHead != head {
		t.Fatalf("branch = %+v, worktree head %s", br, head)
	}
	if got := git(t, e.wt, "show", "HEAD:shared.txt"); got+"\n" != resolved {
		t.Fatalf("shared.txt = %q", got)
	}
	atts, _ := e.o.Store.Attempts(ctx, taskID)
	var resolver bool
	for _, a := range atts {
		if a.Role == "resolver" && a.Usage.InputTokens == 50 {
			resolver = true
		}
	}
	if !resolver {
		t.Fatalf("attempts = %+v, want the resolver attempt", atts)
	}
	if u, _ := e.o.Store.TaskUsage(ctx, taskID); u.CostUSD < 0.03 {
		t.Fatalf("task spend = %+v, want the resolver's cost", u)
	}
	if tk, _ := e.o.Store.GetTask(ctx, taskID); tk.Status != store.TaskVerified {
		t.Fatalf("status = %s", tk.Status)
	}
	if by := e.restackedBy(t); len(by) != 1 || by[0] != "agent" {
		t.Fatalf("restacked events = %v", by)
	}
}

func TestFailedResolutionBlocksTaskAndRestoresBranch(t *testing.T) {
	cases := map[string]runner.Step{
		"resolver fails":       {Error: "agent crashed"},
		"markers are left":     resolverStep(nil),
		"resolver over budget": {RateLimit: &runner.RateLimit{Type: "five_hour"}},
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t, "true", runner.Script{runner.RoleResolver: {step}})
			e.moveMain(t, "shared.txt", mainSide)
			res := e.pass(t)
			if len(res.Blocked) != 1 || len(res.Rebased) != 0 {
				t.Fatalf("result = %+v\n%s", res, e.out)
			}
			reason := res.Blocked[0].Reason
			if !strings.Contains(reason, "shared.txt") || !strings.Contains(reason, "resolver") || !strings.Contains(reason, "mill task rebase") {
				t.Fatalf("reason = %q", reason)
			}
			tk, _ := e.o.Store.GetTask(context.Background(), taskID)
			if tk.Status != store.TaskBlocked || tk.Reason != reason {
				t.Fatalf("task = %s %q", tk.Status, tk.Reason)
			}
			e.assertUnchanged(t)
		})
	}
}

func TestVerifierFailAfterResolutionBlocksTaskAndRestoresBranch(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"shared.txt": resolved})},
		runner.RoleVerifier: {verdictStep("fail")},
	})
	e.moveMain(t, "shared.txt", mainSide)
	res := e.pass(t)
	if len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0].Reason, "verifier") || len(res.Rebased) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	e.assertUnchanged(t)
}

func TestRedGateAfterRestackRunsFixer(t *testing.T) {
	e := setup(t, "grep -q fixed shared.txt", runner.Script{
		runner.RoleImplementer: {implStep(map[string]string{"shared.txt": resolved + "fixed\n"})},
		runner.RoleVerifier:    {verdictStep("pass")},
	})
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)

	res := e.pass(t)
	if len(res.Fixed) != 1 || res.Fixed[0] != taskID || len(res.ResolvedRerere) != 1 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	var implPrompt string
	for _, c := range e.fake.Calls() {
		if c.Role == runner.RoleImplementer {
			implPrompt = c.Prompt
		}
	}
	if !strings.Contains(implPrompt, "[gate] `grep -q fixed shared.txt` failed") {
		t.Fatalf("fixer prompt does not carry the gate failure:\n%s", implPrompt)
	}
	ctx := context.Background()
	tk, _ := e.o.Store.GetTask(ctx, taskID)
	if tk.Status != store.TaskVerified {
		t.Fatalf("status = %s (%s)", tk.Status, tk.Reason)
	}
	head := git(t, e.wt, "rev-parse", "HEAD")
	if br, _ := e.o.Store.GetBranch(ctx, taskID); br.HeadSHA != head || br.StartSHA != tip {
		t.Fatalf("branch = %+v, head %s", br, head)
	}
}

func TestRebasedPublishedTaskIsRepublished(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	pr, err := publish.Publish(ctx, publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)

	res := e.pass(t)
	if len(res.Rebased) != 1 || len(res.Republished) != 1 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	br, _ := e.o.Store.GetBranch(ctx, taskID)
	remote := git(t, e.bare, "rev-parse", "refs/heads/"+publish.ReviewRef(taskID))
	if remote == pr.SHA || remote != br.PublishedSHA || remote != res.Republished[0].SHA {
		t.Fatalf("remote review branch = %s, first publish %s, recorded %s", remote, pr.SHA, br.PublishedSHA)
	}
	if git(t, e.root, "rev-parse", remote+"^{tree}") != git(t, e.root, "rev-parse", br.HeadSHA+"^{tree}") {
		t.Fatalf("republished tree differs from the new head")
	}
	if parent := git(t, e.root, "rev-parse", remote+"^"); parent != tip {
		t.Fatalf("republished parent = %s, want %s", parent, tip)
	}
	if tk, _ := e.o.Store.GetTask(ctx, taskID); tk.Status != store.TaskPublished {
		t.Fatalf("status = %s", tk.Status)
	}
}

func TestPassLeavesBusyTaskUnchangedAndReportsIt(t *testing.T) {
	e := setup(t, "true", runner.Script{})
	ctx := context.Background()
	aid, err := e.o.Store.StartAttempt(ctx, taskID, 2, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.o.Store.SetAttemptProcess(ctx, aid, 4242, "claude", ""); err != nil {
		t.Fatal(err)
	}
	tip := e.moveMain(t, "shared.txt", mainSide)
	e.recordResolution(t, tip)

	res := e.pass(t)
	if len(res.Busy) != 1 || res.Busy[0].TaskID != taskID || res.Busy[0].Doing != "attempt 2 (reviewer) running" ||
		len(res.Rebased) != 0 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	e.assertUnchanged(t)
	if tk, _ := e.o.Store.GetTask(ctx, taskID); tk.Status != store.TaskVerified {
		t.Errorf("status = %s", tk.Status)
	}
	running, err := e.o.Store.RunningAttempts(ctx)
	if err != nil || len(running) != 1 || running[0].ID != aid || running[0].PID != 4242 {
		t.Errorf("running attempts = %+v, %v; want attempt %d with pid 4242 untouched", running, err, aid)
	}
	if claims, err := e.o.Store.ClaimedTasks(ctx); err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

// revertingAttempts commits attempts 2 and 3 on the branch checked out in wt: attempt 2 writes
// name, attempt 3 deletes it again. It records the new head of task and returns it.
func (e *env) revertingAttempts(t *testing.T, task, wt, name string) string {
	t.Helper()
	ctx := context.Background()
	commit := func(n int) {
		git(t, wt, "add", "-A")
		git(t, wt, "commit", "-q", "-m", fmt.Sprintf("mill: %s attempt %d", task, n), "-m", fmt.Sprintf("Mill-Task: %s\nMill-Attempt: %d", task, n))
	}
	if err := os.WriteFile(filepath.Join(wt, name), []byte("from "+task+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(2)
	if err := os.Remove(filepath.Join(wt, name)); err != nil {
		t.Fatal(err)
	}
	commit(3)
	head := git(t, wt, "rev-parse", "HEAD")
	br, err := e.o.Store.GetBranch(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	br.HeadSHA, br.GateSHA = head, head
	if err := e.o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestRebaseReplaysNetChangeSoRevertedAttemptsDoNotConflict(t *testing.T) {
	// Only shared.txt really conflicts; the resolver has a single step for it.
	e := setup(t, "grep -q 'main task' shared.txt", runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"shared.txt": resolved})},
		runner.RoleVerifier: {verdictStep("pass")},
	})
	e.head = e.revertingAttempts(t, taskID, e.wt, "other.txt")
	e.moveMain(t, "other.txt", "main other\n")
	tip := e.moveMain(t, "shared.txt", mainSide)

	res := e.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].ResolvedBy != ResolvedByAgent || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if n := e.roleCalls(runner.RoleResolver); n != 1 {
		t.Fatalf("resolver calls = %d, want 1", n)
	}
	head := git(t, e.wt, "rev-parse", "HEAD")
	if parent := git(t, e.wt, "rev-parse", "HEAD^"); parent != tip {
		t.Fatalf("replayed commit's parent = %s, want %s", parent, tip)
	}
	if files := git(t, e.wt, "diff", "--name-only", tip, head); files != "shared.txt" {
		t.Fatalf("net change = %q, want shared.txt only", files)
	}
	if got := git(t, e.wt, "show", "HEAD:other.txt"); got != "main other" {
		t.Fatalf("other.txt = %q, want main's", got)
	}
	if subject := git(t, e.wt, "log", "-1", "--format=%s"); subject != "mill: g1/a attempts 2-3 (squashed for restack)" {
		t.Fatalf("replayed commit subject = %q", subject)
	}
}

func TestRebaseBlocksWhenNetChangeConflictsAndResolverFails(t *testing.T) {
	e := setup(t, "true", runner.Script{
		runner.RoleResolver: {resolverStep(nil)}, // leaves the conflict markers
	})
	e.head = e.revertingAttempts(t, taskID, e.wt, "other.txt")
	e.moveMain(t, "shared.txt", mainSide)

	res := e.pass(t)
	if len(res.Rebased) != 0 || len(res.Blocked) != 1 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if tk, _ := e.o.Store.GetTask(context.Background(), taskID); tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "shared.txt") {
		t.Fatalf("task = %s %q", tk.Status, tk.Reason)
	}
	e.assertUnchanged(t)
}
