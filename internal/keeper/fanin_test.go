package keeper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const fanInID = "g2/c"

// fanIn is a second goal on top of setup's repo: tasks a and b off main, and c depending on
// both. b is c's deepest parent (the last listed), so c's branch starts at b's head with a
// merged into it. a changes the last line of fan.txt; c adds c.txt and changes the second line.
type fanIn struct {
	*env
	aWT, bWT, cWT    string
	aHead, bHead     string
	cBase, cStart    string // c's recorded base (b's head) and start (the merge of a)
	cHead            string
	aBranch, bBranch string
}

const fanOriginal = "one\ntwo\nthree\nfour\nfive\n"

func setupFanIn(t *testing.T, checkCmd string, script runner.Script) *fanIn {
	t.Helper()
	e := setup(t, checkCmd, script)
	ctx := context.Background()
	s := e.o.Store
	base := writeCommit(t, e.root, "fan.txt", fanOriginal, "add fan.txt")
	git(t, e.root, "push", "-q", "origin", "main")

	spec := func(id string, deps ...string) taskspec.Spec {
		return taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write " + id,
			Acceptance: []string{"ok"}, Owns: []string{}, Size: "S", DependsOn: deps}
	}
	pl := plan.Plan{Goal: "fan", Tasks: []taskspec.Spec{spec("a"), spec("b"), spec("c", "a", "b")}}
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "fan", raw)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "g2" {
		t.Fatalf("goal id = %s, want g2", g.ID)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}

	f := &fanIn{env: e, aBranch: "mill/g2/a", bBranch: "mill/g2/b"}
	f.aWT, f.bWT, f.cWT = filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b"), filepath.Join(t.TempDir(), "c")
	git(t, e.root, "worktree", "add", "-q", "-b", f.aBranch, f.aWT, base)
	f.aHead = writeCommit(t, f.aWT, "fan.txt", "one\ntwo\nthree\nfour\nfive a\n", "work on a")
	git(t, e.root, "worktree", "add", "-q", "-b", f.bBranch, f.bWT, base)
	f.bHead = writeCommit(t, f.bWT, "b.txt", "b\n", "work on b")
	git(t, e.root, "worktree", "add", "-q", "-b", "mill/g2/c", f.cWT, f.bHead)
	git(t, f.cWT, "merge", "-q", "--no-ff", "-m", "mill: merge mill/g2/a (dependency g2/a) into mill/g2/c", f.aBranch)
	f.cBase = f.bHead
	f.cStart = git(t, f.cWT, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(f.cWT, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.cWT, "add", "c.txt")
	f.cHead = writeCommit(t, f.cWT, "fan.txt", "one\ntwo c\nthree\nfour\nfive a\n", "work on c")

	for _, br := range []store.Branch{
		{TaskID: "g2/a", Name: f.aBranch, Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: f.aHead, GateSHA: f.aHead, Worktree: f.aWT},
		{TaskID: "g2/b", Name: f.bBranch, Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: f.bHead, GateSHA: f.bHead, Worktree: f.bWT},
		{TaskID: fanInID, Name: "mill/g2/c", Base: f.bBranch, BaseSHA: f.cBase, StartSHA: f.cStart, HeadSHA: f.cHead, GateSHA: f.cHead, Worktree: f.cWT},
	} {
		if err := s.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if aid, err := s.StartAttempt(ctx, fanInID, 1, "implementer"); err != nil {
		t.Fatal(err)
	} else if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "task-verdict", g.ID, fanInID, map[string]any{"attempt": 1, "head": f.cHead,
		"verdict": map[string]any{"verdict": "pass", "summary": "fine", "findings": []any{}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// moveParent commits content as name on a parent's branch and records the new head.
func (f *fanIn) moveParent(t *testing.T, id, wt, name, content string) string {
	t.Helper()
	ctx := context.Background()
	head := writeCommit(t, wt, name, content, "move "+id)
	br, err := f.o.Store.GetBranch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	br.HeadSHA, br.GateSHA = head, head
	if err := f.o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	return head
}

func (f *fanIn) gates(t *testing.T) int {
	t.Helper()
	evs, err := f.o.Store.TaskEvents(context.Background(), fanInID)
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

func (f *fanIn) isAncestor(t *testing.T, a, b string) bool {
	t.Helper()
	in, err := gitx.IsAncestor(context.Background(), f.root, a, b)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// assertFanInUnchanged checks c's branch is back at its old head and row, clean.
func (f *fanIn) assertFanInUnchanged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if head := git(t, f.cWT, "rev-parse", "HEAD"); head != f.cHead {
		t.Errorf("head = %s, want the old head %s", head, f.cHead)
	}
	if !gitx.IsWorktreeOn(ctx, f.cWT, "mill/g2/c") {
		t.Error("worktree is not on mill/g2/c")
	}
	br, _ := f.o.Store.GetBranch(ctx, fanInID)
	if br.HeadSHA != f.cHead || br.StartSHA != f.cStart || br.BaseSHA != f.cBase || br.Base != f.bBranch {
		t.Errorf("branch row = %+v", br)
	}
	if dirty, err := gitx.HasChanges(ctx, f.cWT); err != nil || dirty {
		t.Errorf("worktree dirty = %v, %v", dirty, err)
	}
}

func TestFanInTaskWhoseParentsMovedWithoutConflictIsLeftAlone(t *testing.T) {
	f := setupFanIn(t, "true", runner.Script{})
	f.moveParent(t, "g2/b", f.bWT, "b2.txt", "b2\n")
	res := f.pass(t)
	if len(res.Rebased) != 0 || len(res.Blocked) != 0 || len(f.fake.Calls()) != 0 {
		t.Fatalf("result = %+v\n%s", res, f.out)
	}
	f.assertFanInUnchanged(t)
}

func TestFanInTaskIsRestackedOntoDeepestParentsNewHead(t *testing.T) {
	// b's new commit changes the line c changes, so c's own commit conflicts on replay.
	resolvedFan := "one\ntwo b c\nthree\nfour\nfive a\n"
	f := setupFanIn(t, "test -f c.txt && test -f b.txt && grep -q 'two b c' fan.txt && grep -q 'five a' fan.txt", runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"fan.txt": resolvedFan})},
		runner.RoleVerifier: {verdictStep("pass")},
	})
	bNew := f.moveParent(t, "g2/b", f.bWT, "fan.txt", "one\ntwo b\nthree\nfour\nfive\n")

	res := f.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].TaskID != fanInID || res.Rebased[0].ResolvedBy != ResolvedByAgent || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, f.out)
	}
	if f.roleCalls(runner.RoleResolver) != 1 || f.roleCalls(runner.RoleVerifier) != 1 || f.gates(t) != 1 {
		t.Fatalf("resolver %d, verifier %d, gates %d", f.roleCalls(runner.RoleResolver), f.roleCalls(runner.RoleVerifier), f.gates(t))
	}
	ctx := context.Background()
	head := git(t, f.cWT, "rev-parse", "HEAD")
	br, _ := f.o.Store.GetBranch(ctx, fanInID)
	if br.HeadSHA != head || br.GateSHA != head || br.BaseSHA != bNew || br.Base != f.bBranch || br.StartSHA == f.cStart || head == f.cHead {
		t.Fatalf("branch = %+v, worktree head %s", br, head)
	}
	// The new start is a merged into b's new head; c's own commit is replayed on it.
	if got := git(t, f.root, "rev-list", "--parents", "-n1", br.StartSHA); got != br.StartSHA+" "+bNew+" "+f.aHead {
		t.Fatalf("start parents = %q, want %s %s", got, bNew, f.aHead)
	}
	if !f.isAncestor(t, bNew, head) || !f.isAncestor(t, f.aHead, head) {
		t.Fatal("new head lacks a parent's head")
	}
	if got := git(t, f.root, "rev-list", "--count", br.StartSHA+".."+head); got != "1" {
		t.Fatalf("own commits = %s, want 1", got)
	}
	if got := git(t, f.cWT, "show", "HEAD:fan.txt"); got+"\n" != resolvedFan {
		t.Fatalf("fan.txt = %q", got)
	}
	if tk, _ := f.o.Store.GetTask(ctx, fanInID); tk.Status != store.TaskVerified {
		t.Fatalf("status = %s", tk.Status)
	}
}

// remergeConflict moves b with a commit that changes the line a changed, so merging a again
// onto b's new head conflicts (and so does b's tip with c's head).
func remergeConflict(t *testing.T, script runner.Script) *fanIn {
	t.Helper()
	f := setupFanIn(t, "test -f c.txt && grep -q 'five a b' fan.txt", script)
	f.moveParent(t, "g2/b", f.bWT, "fan.txt", "one\ntwo\nthree\nfour\nfive b\n")
	return f
}

func TestFanInRemergeConflictIsResolvedByResolver(t *testing.T) {
	f := remergeConflict(t, runner.Script{
		runner.RoleResolver: {resolverStep(map[string]string{"fan.txt": "one\ntwo\nthree\nfour\nfive a b\n"})},
		runner.RoleVerifier: {verdictStep("pass")},
	})
	res := f.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].ResolvedBy != ResolvedByAgent || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, f.out)
	}
	if f.roleCalls(runner.RoleResolver) != 1 || f.gates(t) != 1 {
		t.Fatalf("resolver %d, gates %d", f.roleCalls(runner.RoleResolver), f.gates(t))
	}
	head := git(t, f.cWT, "rev-parse", "HEAD")
	if got := git(t, f.cWT, "show", "HEAD:fan.txt"); got != "one\ntwo c\nthree\nfour\nfive a b" {
		t.Fatalf("fan.txt = %q", got)
	}
	br, _ := f.o.Store.GetBranch(context.Background(), fanInID)
	if br.HeadSHA != head || !f.isAncestor(t, f.aHead, head) {
		t.Fatalf("branch = %+v", br)
	}
}

func TestFanInRemergeConflictWithFailedResolverBlocksAndRestoresBranch(t *testing.T) {
	f := remergeConflict(t, runner.Script{runner.RoleResolver: {{Error: "agent crashed"}}})
	res := f.pass(t)
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != fanInID || len(res.Rebased) != 0 {
		t.Fatalf("result = %+v\n%s", res, f.out)
	}
	reason := res.Blocked[0].Reason
	if !strings.Contains(reason, "fan.txt") || !strings.Contains(reason, "resolver") || !strings.Contains(reason, "merging g2/a") {
		t.Fatalf("reason = %q", reason)
	}
	if tk, _ := f.o.Store.GetTask(context.Background(), fanInID); tk.Status != store.TaskBlocked {
		t.Fatalf("status = %s", tk.Status)
	}
	f.assertFanInUnchanged(t)
}

func TestFanInTaskWhoseDeepestParentLandedIsBasedOnRemoteBase(t *testing.T) {
	f := setupFanIn(t, "test -f c.txt && test -f b.txt && grep -q 'five a' fan.txt", runner.Script{})
	ctx := context.Background()
	// b lands on the remote base branch.
	git(t, f.root, "merge", "-q", "--no-ff", "-m", "land b", f.bBranch)
	landed := git(t, f.root, "rev-parse", "HEAD")
	git(t, f.root, "push", "-q", "origin", "main")
	if err := f.o.Store.SetTaskStatus(ctx, "g2/b", store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}

	res := f.pass(t)
	if len(res.Rebased) != 1 || res.Rebased[0].Onto != "origin/main" || res.Rebased[0].OntoSHA != landed || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v\n%s", res, f.out)
	}
	if len(f.fake.Calls()) != 0 || f.gates(t) != 1 {
		t.Fatalf("agent calls %d, gates %d", len(f.fake.Calls()), f.gates(t))
	}
	head := git(t, f.cWT, "rev-parse", "HEAD")
	br, _ := f.o.Store.GetBranch(ctx, fanInID)
	if br.Base != "main" || br.BaseSHA != landed || br.HeadSHA != head || br.GateSHA != head {
		t.Fatalf("branch = %+v", br)
	}
	// a (not landed) is merged again; the landed b is not merged: the start's parents are
	// the remote base tip and a's head.
	if got := git(t, f.root, "rev-list", "--parents", "-n1", br.StartSHA); got != br.StartSHA+" "+landed+" "+f.aHead {
		t.Fatalf("start parents = %q, want %s %s", got, landed, f.aHead)
	}
	if got := git(t, f.root, "log", "--format=%s", landed+".."+head); strings.Contains(got, "dependency g2/b") {
		t.Fatalf("b merged again:\n%s", got)
	}
	if got := git(t, f.root, "rev-list", "--count", br.StartSHA+".."+head); got != "1" {
		t.Fatalf("own commits = %s, want 1", got)
	}

	// The next pass has nothing to do.
	res = f.pass(t)
	if len(res.Rebased) != 0 || len(res.Blocked) != 0 {
		t.Fatalf("second pass = %+v\n%s", res, f.out)
	}
}
