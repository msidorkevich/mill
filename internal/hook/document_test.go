package hook_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/publish"
	"github.com/msidorkevich/mill/internal/skiptest"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeCommit(t *testing.T, dir, name, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

type env struct {
	opts publish.Options
	bare string
}

// stack is a repo with a bare remote and goal g1 with verified task a and task b stacked on it.
// a has a gate event, a verdict, an attempt costing $1.25 and a skip recorded under the old
// event kind; b has a verdict and no gate event (only its recorded gate sha).
func stack(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	writeCommit(t, root, "README", "init")
	base := git(t, root, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pl := plan.Plan{Goal: "stack", Tasks: []taskspec.Spec{
		{ID: "a", Title: "Task a", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"},
		{ID: "b", Title: "Task b", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S", DependsOn: []string{"a"}},
	}}
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	aWT, bWT := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/a", aWT, base)
	aHead := writeCommit(t, aWT, "a.txt", "work on a")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/b", bWT, aHead)
	bHead := writeCommit(t, bWT, "b.txt", "work on b")
	for _, br := range []store.Branch{
		{TaskID: "g1/a", Name: "mill/g1/a", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: aHead, GateSHA: aHead, Worktree: aWT},
		{TaskID: "g1/b", Name: "mill/g1/b", Base: "mill/g1/a", BaseSHA: aHead, StartSHA: aHead, HeadSHA: bHead, GateSHA: bHead, Worktree: bWT},
	} {
		if err := s.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, "task-verdict", "", br.TaskID, map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine " + br.TaskID}}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendEvent(ctx, "task-gate", "", "g1/a", map[string]any{"attempt": 1, "head": aHead, "result": gate.Result{Passed: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, pipeline.OldSkippedTestsEvent, "", "g1/a", map[string]any{"attempt": 1, "head": aHead,
		"skips": []skiptest.Skip{{File: "a_test.go", Line: 3, Reason: "flaky"}}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(ctx, "g1/a", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, id, "done", budget.Usage{CostUSD: 1.25}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	return env{opts: publish.Options{Store: s, Cfg: cfg, Paths: paths}, bare: bare}
}

func (e env) publish(t *testing.T, id string) publish.Result {
	t.Helper()
	o := e.opts
	o.TaskID = id
	res, err := publish.Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e env) build(t *testing.T, id, event, previousBase string) (hook.Document, map[string]any) {
	t.Helper()
	d, err := hook.Build(context.Background(), e.opts.Store, e.opts.Cfg, e.opts.Paths.Root, id, event, previousBase)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return d, m
}

func TestBuildDocumentForBottomTaskHasEveryField(t *testing.T) {
	t.Parallel()
	e := stack(t)
	res := e.publish(t, "g1/a")
	d, m := e.build(t, "g1/a", hook.EventPublish, "")

	body := git(t, e.bare, "log", "-1", "--format=%b", res.SHA)
	aBr, err := e.opts.Store.GetBranch(context.Background(), "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	want := hook.Document{
		Schema: "mill/hook@1", Event: "publish", GoalID: "g1", TaskID: "g1/a", Title: "Task a", Description: body,
		ReviewBranch: "review/g1/a", BaseBranch: "main", HeadSHA: res.SHA, Remote: "origin", RemoteURL: e.bare,
		StackPosition: 1, Verifier: hook.Verifier{Verdict: "pass", Summary: "fine g1/a"},
		Gate: hook.Gate{Passed: true, SHA: aBr.HeadSHA}, CostUSD: 1.25,
		SkippedTests: []skiptest.Skip{{File: "a_test.go", Line: 3, Reason: "flaky"}},
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("document =\n%+v\nwant\n%+v", d, want)
	}
	if !strings.Contains(d.Description, "Mill-Task: g1/a") || strings.HasPrefix(d.Description, "Task a") {
		t.Errorf("description is not the commit body:\n%s", d.Description)
	}
	for _, k := range []string{"schema", "event", "goal_id", "task_id", "title", "description", "review_branch", "base_branch",
		"head_sha", "remote", "remote_url", "stack_position", "verifier", "gate", "cost_usd", "skipped_tests"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON lacks %q: %v", k, m)
		}
	}
	for _, k := range []string{"previous_base", "parent_task_id", "parent_review_branch"} {
		if _, ok := m[k]; ok {
			t.Errorf("JSON of a bottom task with no previous base has %q", k)
		}
	}
}

func TestBuildDocumentForStackedChildNamesItsParent(t *testing.T) {
	t.Parallel()
	e := stack(t)
	e.publish(t, "g1/a")
	res := e.publish(t, "g1/b")
	d, m := e.build(t, "g1/b", hook.EventUpdate, "main")

	if d.ParentTaskID != "g1/a" || d.ParentReviewBranch != "review/g1/a" || d.StackPosition != 2 || d.BaseBranch != "review/g1/a" {
		t.Errorf("stack fields: parent %q %q, position %d, base %q", d.ParentTaskID, d.ParentReviewBranch, d.StackPosition, d.BaseBranch)
	}
	if d.Event != "update" || d.ReviewBranch != "review/g1/b" || d.HeadSHA != res.SHA || d.Schema != "mill/hook@1" {
		t.Errorf("document = %+v", d)
	}
	if body := git(t, e.bare, "log", "-1", "--format=%b", res.SHA); d.Description != body {
		t.Errorf("description = %q, want the commit body %q", d.Description, body)
	}
	bBr, _ := e.opts.Store.GetBranch(context.Background(), "g1/b")
	if d.Gate != (hook.Gate{Passed: true, SHA: bBr.HeadSHA}) || d.CostUSD != 0 || d.SkippedTests == nil || len(d.SkippedTests) != 0 {
		t.Errorf("gate %+v, cost %v, skips %#v", d.Gate, d.CostUSD, d.SkippedTests)
	}
	if m["previous_base"] != "main" || m["parent_task_id"] != "g1/a" || m["stack_position"] != float64(2) {
		t.Errorf("JSON = %v", m)
	}
}

func TestBuildRefusesUnpublishedTask(t *testing.T) {
	t.Parallel()
	e := stack(t)
	_, err := hook.Build(context.Background(), e.opts.Store, e.opts.Cfg, e.opts.Paths.Root, "g1/b", hook.EventPublish, "")
	if err == nil {
		t.Fatal("Build of an unpublished task succeeded")
	}
	if msg := err.Error(); !strings.Contains(msg, "g1/b") || !strings.Contains(msg, "not published") || !strings.Contains(msg, "mill review approve g1/b") {
		t.Errorf("error = %v", err)
	}
}

func TestStatusBuildDocumentHasStatusEventAndPublishedCommit(t *testing.T) {
	t.Parallel()
	e := stack(t)
	res := e.publish(t, "g1/a")
	d, m := e.build(t, "g1/a", hook.EventStatus, "")
	if d.Event != "status" || m["event"] != "status" || d.Schema != "mill/hook@1" {
		t.Errorf("event %q (JSON %v), schema %q; want status, mill/hook@1", d.Event, m["event"], d.Schema)
	}
	if d.HeadSHA != res.SHA || m["head_sha"] != res.SHA {
		t.Errorf("head_sha = %q, want the published commit %q", d.HeadSHA, res.SHA)
	}
}
