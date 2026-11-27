package keeper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

const ciFixInterval = 2 * time.Minute

// ciFixEnv is a keeper env whose task g1/a is published, with a scripted status hook and an
// injected clock.
type ciFixEnv struct {
	*env
	script string // the JSON the status hook prints; missing makes the hook fail
	calls  string // one line per status hook run: the head_sha it was asked about
	now    time.Time
}

// ciFixSetup publishes g1/a and configures a status hook that logs the head it is asked about
// and prints the scripted document. ciFix sets ci_fix.
func ciFixSetup(t *testing.T, checkCmd string, script runner.Script, ciFix bool) *ciFixEnv {
	t.Helper()
	e := &ciFixEnv{env: setup(t, checkCmd, script), now: time.Unix(0, 0)}
	dir := t.TempDir()
	e.script, e.calls = filepath.Join(dir, "status.json"), filepath.Join(dir, "calls")
	if _, err := publish.Publish(context.Background(), publish.Options{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Host: e.o.Host, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	e.o.Cfg.OnUpdate = "echo https://example.test/pr/1"
	e.o.Cfg.Status = `{ sed -n 's/.*"head_sha":"\([^"]*\)".*/\1/p'; echo; } >> '` + e.calls + `'; cat '` + e.script + `' || exit 4`
	e.o.Cfg.StatusInterval = config.Duration{Duration: ciFixInterval}
	e.o.Cfg.CIFix = ciFix
	e.o.Now = func() time.Time { return e.now }
	return e
}

// ciFixScript makes the status hook report state with checks.
func (e *ciFixEnv) ciFixScript(t *testing.T, state string, checks ...hook.StatusCheck) {
	t.Helper()
	if checks == nil {
		checks = []hook.StatusCheck{}
	}
	b, err := json.Marshal(hook.Status{Schema: hook.StatusSchema, State: state, URL: "https://example.test/pr/1", Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.script, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ciFixQueried lists the heads the status hook was asked about.
func (e *ciFixEnv) ciFixQueried(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// ciFixPublished is the published head of g1/a.
func (e *ciFixEnv) ciFixPublished(t *testing.T) string {
	t.Helper()
	br, err := e.o.Store.GetBranch(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return br.PublishedSHA
}

// ciFixImplPrompts are the implementer prompts the fake runner got.
func (e *ciFixEnv) ciFixImplPrompts() []string {
	var out []string
	for _, c := range e.fake.Calls() {
		if c.Role == runner.RoleImplementer {
			out = append(out, c.Prompt)
		}
	}
	return out
}

func ciFixStatus(t *testing.T, e *ciFixEnv) store.Task {
	t.Helper()
	tk, err := e.o.Store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

var ciFixChecks = []hook.StatusCheck{
	{Name: "lint", State: "success", URL: "https://ci.example/lint", Summary: "clean"},
	{Name: "unit", State: "failure", URL: "https://ci.example/unit", Summary: "FAIL TestHello"},
	{Name: "e2e", State: "failure", URL: "https://ci.example/e2e", Summary: "timeout in login"},
}

func TestCIFixDisabledRecordsFailureWithoutFixer(t *testing.T) {
	t.Parallel()
	e := ciFixSetup(t, "true", runner.Script{}, false)
	e.ciFixScript(t, "failure", ciFixChecks...)
	pub := e.ciFixPublished(t)

	res := e.pass(t)
	if len(res.CI) != 1 || res.CI[0].State != "failure" || res.CI[0].SHA != pub {
		t.Fatalf("CI changes = %+v\n%s", res.CI, e.out)
	}
	if !strings.Contains(e.out.String(), taskID+": CI failure at "+short(pub)+": unit, e2e") {
		t.Fatalf("output does not show the failure:\n%s", e.out)
	}
	if n := len(e.fake.Calls()); n != 0 || len(res.Fixed) != 0 || len(res.Blocked) != 0 || len(res.Republished) != 0 {
		t.Fatalf("agent calls = %d, result = %+v; want no fixer", n, res)
	}
	if tk := ciFixStatus(t, e); tk.Status != store.TaskPublished {
		t.Fatalf("status = %s (%s), want published", tk.Status, tk.Reason)
	}
}

func TestCIFixFailureAtCurrentHeadIsFixedRepublishedAndQueriedAgain(t *testing.T) {
	t.Parallel()
	e := ciFixSetup(t, "grep -q 'b task' shared.txt", runner.Script{
		runner.RoleImplementer: {implStep(map[string]string{"shared.txt": taskSide + "fixed\n"})},
		runner.RoleVerifier:    {verdictStep("pass")},
	}, true)
	e.ciFixScript(t, "failure", ciFixChecks...)
	old := e.ciFixPublished(t)

	res := e.pass(t)
	if len(res.Fixed) != 1 || res.Fixed[0] != taskID || len(res.Republished) != 1 || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	ip := e.ciFixImplPrompts()
	if len(ip) != 1 {
		t.Fatalf("implementer prompts = %d, want 1\n%s", len(ip), e.out)
	}
	for _, want := range []string{"[ci] CI check `unit` failed", "https://ci.example/unit", "FAIL TestHello",
		"[ci] CI check `e2e` failed", "https://ci.example/e2e", "timeout in login"} {
		if !strings.Contains(ip[0], want) {
			t.Fatalf("fixer prompt lacks %q:\n%s", want, ip[0])
		}
	}
	if strings.Contains(ip[0], "ci.example/lint") {
		t.Fatalf("fixer prompt carries the passing check:\n%s", ip[0])
	}
	if e.roleCalls(runner.RoleVerifier) != 1 {
		t.Fatalf("verifier runs = %d, want 1", e.roleCalls(runner.RoleVerifier))
	}

	ctx := context.Background()
	br, _ := e.o.Store.GetBranch(ctx, taskID)
	head := git(t, e.wt, "rev-parse", "HEAD")
	if br.HeadSHA != head || br.GateSHA != head || head == e.head {
		t.Fatalf("branch = %+v, worktree head %s; want gated at the fixed head", br, head)
	}
	remote := git(t, e.bare, "rev-parse", "refs/heads/"+publish.ReviewRef(taskID))
	if remote == old || remote != br.PublishedSHA || remote != res.Republished[0].SHA {
		t.Fatalf("review branch = %s, old %s, recorded %s", remote, old, br.PublishedSHA)
	}
	if tk := ciFixStatus(t, e); tk.Status != store.TaskPublished {
		t.Fatalf("status = %s (%s), want published", tk.Status, tk.Reason)
	}
	hookOrderAfter(t, e.out.String(), taskID+": republished ", "hook on_update for "+taskID+": https://example.test/pr/1\n")

	// The next pass queries CI at the new published head, without waiting for the interval.
	e.ciFixScript(t, "success")
	res = e.pass(t)
	if q := e.ciFixQueried(t); len(q) != 2 || q[0] != old || q[1] != br.PublishedSHA {
		t.Fatalf("status hook queried %v, want %s then %s", q, old, br.PublishedSHA)
	}
	if len(res.CI) != 1 || res.CI[0].State != "success" || res.CI[0].SHA != br.PublishedSHA || len(e.ciFixImplPrompts()) != 1 {
		t.Fatalf("second pass CI = %+v, implementer runs %d", res.CI, len(e.ciFixImplPrompts()))
	}
}

func TestCIFixIgnoresFailureOfOlderHead(t *testing.T) {
	t.Parallel()
	e := ciFixSetup(t, "true", runner.Script{}, false)
	e.ciFixScript(t, "failure", ciFixChecks...)
	old := e.ciFixPublished(t)
	if res := e.pass(t); len(res.CI) != 1 || res.CI[0].SHA != old {
		t.Fatalf("CI changes = %+v\n%s", res.CI, e.out)
	}

	// The task is restacked and re-published; the hook then fails, so the latest record stays
	// the failure of the old head.
	e.recordResolution(t, e.moveMain(t, "shared.txt", mainSide))
	if err := os.Remove(e.script); err != nil {
		t.Fatal(err)
	}
	e.o.Cfg.CIFix = true
	res := e.pass(t)
	if len(res.Republished) != 1 || e.ciFixPublished(t) == old {
		t.Fatalf("result = %+v\n%s", res, e.out)
	}
	if n := len(e.fake.Calls()); n != 0 || len(res.Fixed) != 0 || len(res.Blocked) != 0 {
		t.Fatalf("agent calls = %d, result = %+v; want the old head's failure ignored", n, res)
	}
	if tk := ciFixStatus(t, e); tk.Status != store.TaskPublished {
		t.Fatalf("status = %s (%s), want published", tk.Status, tk.Reason)
	}
}

func TestCIFixAttemptCapBlocksWithCIReasonAndSameFailureIsNotFixedTwice(t *testing.T) {
	t.Parallel()
	e := ciFixSetup(t, "test -f fixed.txt || ! test -f broken.txt", runner.Script{
		runner.RoleImplementer: {implStep(map[string]string{"broken.txt": "still broken\n"})},
	}, true)
	e.o.Cfg.MaxAttempts = 1
	e.o.Pipeline.Cfg.MaxAttempts = 1
	e.ciFixScript(t, "failure", ciFixChecks...)
	pub := e.ciFixPublished(t)

	res := e.pass(t)
	want := "keeper: CI failed at " + short(pub) + ": unit, e2e"
	if len(res.Blocked) != 1 || !strings.HasPrefix(res.Blocked[0].Reason, want) || len(res.Fixed) != 0 || len(res.Republished) != 0 {
		t.Fatalf("result = %+v, want blocked with %q\n%s", res, want, e.out)
	}
	tk := ciFixStatus(t, e)
	if tk.Status != store.TaskBlocked || !strings.HasPrefix(tk.Reason, want) || !strings.Contains(tk.Reason, "not verified after 1 attempt(s)") {
		t.Fatalf("task = %s %q", tk.Status, tk.Reason)
	}
	if n := len(e.ciFixImplPrompts()); n != 1 {
		t.Fatalf("implementer runs = %d, want 1 (the attempt cap)", n)
	}

	// Put back as published at the same head: the same failure record is not acted on again,
	// even when the hook is queried again and reports it unchanged.
	if err := e.o.Store.SetTaskStatus(context.Background(), taskID, store.TaskPublished, "", ""); err != nil {
		t.Fatal(err)
	}
	git(t, e.wt, "reset", "-q", "--hard", e.head)
	git(t, e.wt, "clean", "-fdq")
	e.now = e.now.Add(ciFixInterval)
	res = e.pass(t)
	if q := e.ciFixQueried(t); len(q) != 2 || q[1] != pub {
		t.Fatalf("status hook queried %v, want %s twice", q, pub)
	}
	if n := len(e.ciFixImplPrompts()); n != 1 || len(res.Blocked) != 0 || len(res.Fixed) != 0 || len(res.CI) != 0 {
		t.Fatalf("implementer runs = %d, result = %+v; want the failure not fixed twice", n, res)
	}
	if tk := ciFixStatus(t, e); tk.Status != store.TaskPublished {
		t.Fatalf("status = %s (%s), want published", tk.Status, tk.Reason)
	}
}
