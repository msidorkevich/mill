package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
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

// fixture is a repo with a bare remote and one verified task per id, each on its own branch
// mill/t/<id> off main with one commit adding <id>.txt.
func fixture(t *testing.T, ids ...string) (Options, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	base := writeCommit(t, root, "README", "init")
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
	for _, id := range ids {
		wt := filepath.Join(t.TempDir(), id)
		name := "mill/t/" + id
		git(t, root, "worktree", "add", "-q", "-b", name, wt, base)
		head := writeCommit(t, wt, id+".txt", "work on "+id)
		spec := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Size: "S"}
		if err := s.CreateTask(ctx, spec); err != nil {
			t.Fatal(err)
		}
		br := store.Branch{TaskID: id, Name: name, Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: head, GateSHA: head, Worktree: wt}
		if err := s.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, id, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	return Options{Store: s, Cfg: cfg, Paths: paths}, bare
}

func TestSyncMarksVerifiedBranchMergedDirectlyAsLanded(t *testing.T) {
	o, bare := fixture(t, "merged", "pending")
	ctx := context.Background()

	// Someone merges the task branch itself (not a published review) into the remote main.
	forge := filepath.Join(t.TempDir(), "forge")
	git(t, t.TempDir(), "clone", "-q", bare, forge)
	git(t, forge, "checkout", "-q", "-B", "main", "origin/main")
	git(t, o.Paths.Root, "push", "-q", bare, "mill/t/merged:refs/heads/feature")
	git(t, forge, "fetch", "-q", "origin")
	git(t, forge, "merge", "-q", "--no-ff", "-m", "Merge feature", "origin/feature")
	merge := git(t, forge, "rev-parse", "HEAD")
	git(t, forge, "push", "-q", "origin", "main")

	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 1 || res.Landed[0] != (Landed{TaskID: "merged", Method: MethodMerged, Commit: merge}) {
		t.Fatalf("landed = %+v", res.Landed)
	}
	if res.BaseSHA != merge {
		t.Errorf("base sha = %s, want %s", res.BaseSHA, merge)
	}
	if tk, _ := o.Store.GetTask(ctx, "merged"); tk.Status != store.TaskLanded {
		t.Errorf("merged status = %s", tk.Status)
	}
	if tk, _ := o.Store.GetTask(ctx, "pending"); tk.Status != store.TaskVerified {
		t.Errorf("pending status = %s", tk.Status)
	}
	evs, err := o.Store.TaskEvents(ctx, "merged")
	if err != nil {
		t.Fatal(err)
	}
	var last store.Event
	for _, e := range evs {
		if e.Kind == "task-landed" {
			last = e
		}
	}
	var d map[string]string
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatal(err)
	}
	br, _ := o.Store.GetBranch(ctx, "merged")
	if last.Kind != "task-landed" || d["method"] != "merged" || d["commit"] != merge || d["head_sha"] != br.HeadSHA {
		t.Errorf("last event = %s %v", last.Kind, d)
	}

	// A further sync finds nothing new.
	res, err = Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 0 {
		t.Errorf("second sync landed = %+v", res.Landed)
	}
}

func TestSyncDoesNotLandVerifiedTaskWithoutOwnCommits(t *testing.T) {
	o, _ := fixture(t, "empty")
	ctx := context.Background()
	br, _ := o.Store.GetBranch(ctx, "empty")
	br.HeadSHA, br.GateSHA = br.BaseSHA, br.BaseSHA
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 0 {
		t.Errorf("landed = %+v", res.Landed)
	}
	if tk, _ := o.Store.GetTask(ctx, "empty"); tk.Status != store.TaskVerified {
		t.Errorf("status = %s", tk.Status)
	}
}

// stackedGoal is a goal with task a and task b depending on it, b based on a's head, and a
// moved by one more commit adding a2.txt so that sync restacks b onto it.
func stackedGoal(t *testing.T, reportRedGate bool) Options {
	t.Helper()
	o, _ := fixture(t)
	ctx := context.Background()
	root := o.Paths.Root
	base := git(t, root, "rev-parse", "HEAD")
	pl := plan.Plan{Goal: "stack", Tasks: []taskspec.Spec{
		{ID: "a", Title: "Task a", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"},
		{ID: "b", Title: "Task b", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S", DependsOn: []string{"a"}},
	}}
	raw, _ := json.Marshal(pl)
	g, err := o.Store.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Store.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	aWT, bWT := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/a", aWT, base)
	aHead := writeCommit(t, aWT, "a.txt", "work on a")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/b", bWT, aHead)
	bHead := writeCommit(t, bWT, "b.txt", "work on b")
	aNew := writeCommit(t, aWT, "a2.txt", "more work on a")
	for _, br := range []store.Branch{
		{TaskID: "g1/a", Name: "mill/g1/a", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: aNew, GateSHA: aNew, Worktree: aWT},
		{TaskID: "g1/b", Name: "mill/g1/b", Base: "mill/g1/a", BaseSHA: aHead, StartSHA: aHead, HeadSHA: bHead, GateSHA: bHead, Worktree: bWT},
	} {
		if err := o.Store.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := o.Store.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	o.Cfg.CheckCmd = "test ! -f a2.txt" // red once b sits on a's new head
	o.ReportRedGate = reportRedGate
	return o
}

func TestSyncBlocksTaskWithRedGateAfterRestackByDefault(t *testing.T) {
	o := stackedGoal(t, false)
	res, err := Sync(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || len(res.Blocked) != 1 || res.Blocked[0].TaskID != "g1/b" || res.GateRed != nil {
		t.Fatalf("result = %+v", res)
	}
	if tk, _ := o.Store.GetTask(context.Background(), "g1/b"); tk.Status != store.TaskBlocked {
		t.Fatalf("status = %s", tk.Status)
	}
}

func TestSyncReportsRedGateAfterRestackWithoutBlocking(t *testing.T) {
	o := stackedGoal(t, true)
	ctx := context.Background()
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || len(res.Blocked) != 0 || len(res.GateRed) != 1 || res.GateRed[0].TaskID != "g1/b" ||
		res.GateRed[0].Gate.Passed || len(res.GateRed[0].Gate.Commands) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if tk, _ := o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskVerified {
		t.Fatalf("status = %s", tk.Status)
	}
	br, _ := o.Store.GetBranch(ctx, "g1/b")
	if br.GateSHA == br.HeadSHA {
		t.Fatalf("gate sha %s recorded as green at the new head", br.GateSHA)
	}
}

func TestSyncLeavesBusyMergedTaskUnchangedAndLandsTheOthers(t *testing.T) {
	o, bare := fixture(t, "busy", "free")
	ctx := context.Background()
	aid, err := o.Store.StartAttempt(ctx, "busy", 2, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	forge := filepath.Join(t.TempDir(), "forge")
	git(t, t.TempDir(), "clone", "-q", bare, forge)
	git(t, forge, "checkout", "-q", "-B", "main", "origin/main")
	for _, id := range []string{"busy", "free"} {
		git(t, o.Paths.Root, "push", "-q", bare, "mill/t/"+id+":refs/heads/"+id)
		git(t, forge, "fetch", "-q", "origin")
		git(t, forge, "merge", "-q", "--no-ff", "-m", "Merge "+id, "origin/"+id)
	}
	git(t, forge, "push", "-q", "origin", "main")

	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 1 || res.Landed[0].TaskID != "free" {
		t.Fatalf("landed = %+v", res.Landed)
	}
	if len(res.Busy) != 1 || res.Busy[0] != (Busy{TaskID: "busy", Doing: "attempt 2 (reviewer) running"}) {
		t.Fatalf("busy = %+v", res.Busy)
	}
	if tk, _ := o.Store.GetTask(ctx, "busy"); tk.Status != store.TaskVerified {
		t.Errorf("busy status = %s, want it unchanged", tk.Status)
	}
	running, err := o.Store.RunningAttempts(ctx)
	if err != nil || len(running) != 1 || running[0].ID != aid {
		t.Errorf("running attempts = %+v, %v", running, err)
	}
	if claims, err := o.Store.ClaimedTasks(ctx); err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

func TestSyncClaimsAreReleasedAfterLanding(t *testing.T) {
	o, bare := fixture(t, "x")
	ctx := context.Background()
	git(t, o.Paths.Root, "push", "-q", bare, "mill/t/x:main")
	res, err := Sync(ctx, o)
	if err != nil || len(res.Landed) != 1 || len(res.Busy) != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if claims, err := o.Store.ClaimedTasks(ctx); err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

// attemptCommit commits name with body in dir as attempt n of task, with mill's trailers. An
// empty body deletes name.
func attemptCommit(t *testing.T, dir, task, name, body string, n int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if body == "" {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", fmt.Sprintf("mill: %s attempt %d", task, n), "-m", fmt.Sprintf("Mill-Task: %s\nMill-Attempt: %d", task, n))
	return git(t, dir, "rev-parse", "HEAD")
}

// revertingGoal is a goal with task a and task b depending on it. b's attempt 1 adds b.txt,
// attempt 2 adds a2.txt (b's version) and, when revert is set, attempt 3 deletes it again.
// a then moves by a commit adding a2.txt with other content, so sync restacks b onto it.
func revertingGoal(t *testing.T, revert bool) (Options, string) {
	t.Helper()
	o, _ := fixture(t)
	ctx := context.Background()
	root := o.Paths.Root
	base := git(t, root, "rev-parse", "HEAD")
	pl := plan.Plan{Goal: "stack", Tasks: []taskspec.Spec{
		{ID: "a", Title: "Task a", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"},
		{ID: "b", Title: "Task b", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S", DependsOn: []string{"a"}},
	}}
	raw, _ := json.Marshal(pl)
	g, err := o.Store.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Store.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	aWT, bWT := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/a", aWT, base)
	aHead := writeCommit(t, aWT, "a.txt", "work on a")
	git(t, root, "worktree", "add", "-q", "-b", "mill/g1/b", bWT, aHead)
	attemptCommit(t, bWT, "g1/b", "b.txt", "b\n", 1)
	bHead := attemptCommit(t, bWT, "g1/b", "a2.txt", "b's a2\n", 2)
	if revert {
		bHead = attemptCommit(t, bWT, "g1/b", "a2.txt", "", 3)
	}
	aNew := writeCommit(t, aWT, "a2.txt", "more work on a")
	for _, br := range []store.Branch{
		{TaskID: "g1/a", Name: "mill/g1/a", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: aNew, GateSHA: aNew, Worktree: aWT},
		{TaskID: "g1/b", Name: "mill/g1/b", Base: "mill/g1/a", BaseSHA: aHead, StartSHA: aHead, HeadSHA: bHead, GateSHA: bHead, Worktree: bWT},
	} {
		if err := o.Store.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := o.Store.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return o, aNew
}

func TestSyncRestackReplaysNetChangeOfAttemptsThatRevertEachOther(t *testing.T) {
	o, aNew := revertingGoal(t, true)
	ctx := context.Background()
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || res.Restacked[0].TaskID != "g1/b" || len(res.Blocked) != 0 {
		t.Fatalf("result = %+v", res)
	}
	br, _ := o.Store.GetBranch(ctx, "g1/b")
	wt := br.Worktree
	if parent := git(t, wt, "rev-parse", br.HeadSHA+"^"); parent != aNew || br.StartSHA != aNew {
		t.Fatalf("head parent = %s, start = %s, want %s", parent, br.StartSHA, aNew)
	}
	if files := git(t, wt, "diff", "--name-status", aNew, br.HeadSHA); files != "A\tb.txt" {
		t.Fatalf("net change = %q, want only b.txt added", files)
	}
	if got := git(t, wt, "show", br.HeadSHA+":a2.txt"); got != "a2.txt" {
		t.Fatalf("a2.txt = %q, want a's version", got)
	}
	if subject := git(t, wt, "log", "-1", "--format=%s", br.HeadSHA); subject != "mill: g1/b attempts 1-3 (squashed for restack)" {
		t.Fatalf("replayed commit subject = %q", subject)
	}
	if tk, _ := o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskVerified {
		t.Fatalf("status = %s", tk.Status)
	}
}

func TestSyncRestackBlocksWhenNetChangeConflicts(t *testing.T) {
	o, _ := revertingGoal(t, false)
	ctx := context.Background()
	before, _ := o.Store.GetBranch(ctx, "g1/b")
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 0 || len(res.Blocked) != 1 || res.Blocked[0].TaskID != "g1/b" {
		t.Fatalf("result = %+v", res)
	}
	if tk, _ := o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "a2.txt") {
		t.Fatalf("task = %s %q", tk.Status, tk.Reason)
	}
	after, _ := o.Store.GetBranch(ctx, "g1/b")
	if head := git(t, after.Worktree, "rev-parse", "HEAD"); head != before.HeadSHA || after.HeadSHA != before.HeadSHA {
		t.Fatalf("head = %s (recorded %s), want unchanged %s", head, after.HeadSHA, before.HeadSHA)
	}
	if st := git(t, after.Worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean: %q", st)
	}
}

func TestSyncRetriesRestackBlockedTaskAndRestoresItOnceAttemptUndoesConflict(t *testing.T) {
	o, aNew := revertingGoal(t, false)
	ctx := context.Background()
	if res, err := Sync(ctx, o); err != nil || len(res.Blocked) != 1 {
		t.Fatalf("first sync = %+v, %v", res, err)
	}
	if tk, _ := o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskVerified {
		t.Fatalf("task = %s from %q", tk.Status, tk.RestackBlockedFrom)
	}

	// Busy: the retry leaves it alone and reports it.
	aid, err := o.Store.StartAttempt(ctx, "g1/b", 9, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ctx, o)
	if err != nil || len(res.Busy) != 1 || res.Busy[0].TaskID != "g1/b" || len(res.Blocked)+len(res.Restacked) != 0 {
		t.Fatalf("busy sync = %+v, %v", res, err)
	}
	if err := o.Store.FinishAttempt(ctx, aid, "ok", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}

	// A later attempt undoes the conflicting change.
	br, _ := o.Store.GetBranch(ctx, "g1/b")
	br.HeadSHA = attemptCommit(t, br.Worktree, "g1/b", "a2.txt", "", 3)
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	res, err = Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restacked) != 1 || len(res.Blocked) != 0 || len(res.Restored) != 1 || res.Restored[0] != (Restored{TaskID: "g1/b", Status: store.TaskVerified}) {
		t.Fatalf("result = %+v", res)
	}
	tk, _ := o.Store.GetTask(ctx, "g1/b")
	if tk.Status != store.TaskVerified || tk.Reason != "" || tk.RestackBlockedFrom != "" {
		t.Fatalf("task = %s %q from %q", tk.Status, tk.Reason, tk.RestackBlockedFrom)
	}
	br, _ = o.Store.GetBranch(ctx, "g1/b")
	if br.StartSHA != aNew || br.GateSHA != br.HeadSHA {
		t.Fatalf("branch = %+v, want started at %s and gated", br, aNew)
	}
}

func TestSyncRetriesRestackBlockedTaskOnlyWhileStillConflicting(t *testing.T) {
	o, _ := revertingGoal(t, false)
	ctx := context.Background()
	if _, err := Sync(ctx, o); err != nil {
		t.Fatal(err)
	}
	// The dependency moves again, still writing a2.txt: the retry blocks with the new reason.
	a, _ := o.Store.GetBranch(ctx, "g1/a")
	a.HeadSHA = writeCommit(t, a.Worktree, "a3.txt", "even more work on a")
	if err := o.Store.SaveBranch(ctx, a); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ctx, o)
	if err != nil || len(res.Blocked) != 1 || len(res.Restacked) != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	tk, _ := o.Store.GetTask(ctx, "g1/b")
	if tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskVerified || !strings.Contains(tk.Reason, short(a.HeadSHA)) {
		t.Fatalf("task = %s from %q: %q", tk.Status, tk.RestackBlockedFrom, tk.Reason)
	}

	// Blocked for another reason: sync leaves it alone, even though the restack would now work.
	if err := o.Store.SetTaskStatus(ctx, "g1/b", store.TaskBlocked, "", "operator rejected"); err != nil {
		t.Fatal(err)
	}
	br, _ := o.Store.GetBranch(ctx, "g1/b")
	br.HeadSHA = attemptCommit(t, br.Worktree, "g1/b", "a2.txt", "", 3)
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	res, err = Sync(ctx, o)
	if err != nil || len(res.Blocked)+len(res.Restacked)+len(res.Restored) != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if tk, _ := o.Store.GetTask(ctx, "g1/b"); tk.Status != store.TaskBlocked || tk.Reason != "operator rejected" {
		t.Fatalf("task = %s %q", tk.Status, tk.Reason)
	}
}
