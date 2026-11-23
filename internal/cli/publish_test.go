package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// addRemote creates a bare repository, adds it as origin and pushes main to it. It returns
// the bare repository's path.
func addRemote(t *testing.T, root string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitC(t, root, "init", "-q", "--bare", bare)
	gitC(t, root, "remote", "add", "origin", bare)
	gitC(t, root, "push", "-q", "origin", "main")
	return bare
}

// remoteRefs maps every ref of the bare remote to its sha.
func remoteRefs(t *testing.T, bare string) map[string]string {
	t.Helper()
	out := gitC(t, bare, "for-each-ref", "--format=%(refname) %(objectname)")
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs
}

// assertOnlyReviewRefsMoved fails if any ref other than refs/heads/review/* differs from before.
func assertOnlyReviewRefsMoved(t *testing.T, bare string, before map[string]string) {
	t.Helper()
	after := remoteRefs(t, bare)
	for name, sha := range after {
		if strings.HasPrefix(name, "refs/heads/review/") {
			continue
		}
		if before[name] != sha {
			t.Errorf("remote ref %s changed: %s -> %s", name, before[name], sha)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("remote ref %s was deleted", name)
		}
	}
}

func publishedGoal(t *testing.T) (root, bare string) {
	t.Helper()
	root = verifiedGoal(t)
	return root, addRemote(t, root)
}

func TestApproveTaskWithoutDependenciesPushesOneSquashedCommit(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "g1/a")

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	pushed := remoteRefs(t, bare)["refs/heads/review/g1/a"]
	if pushed == "" {
		t.Fatalf("review/g1/a not on the remote: %v", remoteRefs(t, bare))
	}
	for _, want := range []string{"review/g1/a", pushed[:8], "pull request from review/g1/a into main"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if parents := gitC(t, bare, "rev-list", "--parents", "-n1", pushed); parents != pushed+" "+br.BaseSHA {
		t.Errorf("parents = %q, want one parent %s", parents, br.BaseSHA)
	}
	if got, want := gitC(t, bare, "rev-parse", pushed+"^{tree}"), gitC(t, root, "rev-parse", br.HeadSHA+"^{tree}"); got != want {
		t.Errorf("tree = %s, want head tree %s", got, want)
	}
	msg := gitC(t, bare, "log", "-1", "--format=%B", pushed)
	for _, want := range []string{"A\n", "Task: g1/a", "Goal: Fan in (g1)", "on main @" + br.BaseSHA[:8], "- [x] a.txt exists",
		"gate passed at " + br.HeadSHA[:8], "test -f a.txt", "Verifier: pass: ok", "attempt(s)",
		"Mill-Task: g1/a\n", "Mill-Goal: g1\n", "Mill-Base: " + br.BaseSHA} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "A\n\n") {
		t.Errorf("first line is not the title:\n%s", msg)
	}
	tk, _ := s.GetTask(ctx, "g1/a")
	after, _ := s.GetBranch(ctx, "g1/a")
	if tk.Status != store.TaskPublished || after.PublishedRef != "review/g1/a" || after.PublishedSHA != pushed {
		t.Errorf("task %s, branch %+v", tk.Status, after)
	}
	evs, _ := s.TaskEvents(ctx, "g1/a")
	if last := evs[len(evs)-1]; last.Kind != "task-published" || !strings.Contains(string(last.Data), pushed) {
		t.Errorf("last event = %+v", last)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveSingleTaskOmitsGoalTrailer(t *testing.T) {
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	spec := `{"id": "solo", "title": "Solo", "kind": "implement", "spec": "write", "acceptance": ["ok"],
  "checks": ["true"], "owns": [], "depends_on": [], "size": "S"}`
	if r := run(t, root, nil, "task", "run", "--file", writeFile(t, spec)); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	bare := addRemote(t, root)
	before := remoteRefs(t, bare)
	if r := run(t, root, nil, "review", "approve", "solo"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	pushed := remoteRefs(t, bare)["refs/heads/review/solo"]
	msg := gitC(t, bare, "log", "-1", "--format=%B", pushed)
	if !strings.Contains(msg, "Mill-Task: solo") || !strings.Contains(msg, "Mill-Base: ") || strings.Contains(msg, "Mill-Goal") {
		t.Errorf("message:\n%s", msg)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveStackedTaskNeedsPublishedParent(t *testing.T) {
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, chainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve plan: %+v", r)
	}
	if r := run(t, root, nil, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	bare := addRemote(t, root)
	before := remoteRefs(t, bare)

	r := run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g2/b: queued, will publish when parent g2/a is published or landed") {
		t.Fatalf("approve before parent: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Fatal("refused approve pushed a branch")
	}

	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("approve parent: %+v", r)
	}
	r = run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "into review/g2/a") {
		t.Fatalf("approve child: %+v", r)
	}
	refs := remoteRefs(t, bare)
	parentPub, childPub := refs["refs/heads/review/g2/a"], refs["refs/heads/review/g2/b"]
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", childPub); got != childPub+" "+parentPub {
		t.Errorf("child parents = %q, want %s", got, parentPub)
	}
	ctx := context.Background()
	b, _ := openStore(t, root).GetBranch(ctx, "g2/b")
	if got, want := gitC(t, bare, "rev-parse", childPub+"^{tree}"), gitC(t, root, "rev-parse", b.HeadSHA+"^{tree}"); got != want {
		t.Errorf("child tree = %s, want %s", got, want)
	}
	msg := gitC(t, bare, "log", "-1", "--format=%B", childPub)
	if !strings.Contains(msg, "on review/g2/a") || !strings.Contains(msg, "Mill-Base: "+parentPub) {
		t.Errorf("child message:\n%s", msg)
	}
	// Only b's own change is in its commit.
	if files := gitC(t, bare, "diff", "--name-only", parentPub, childPub); files != "b.txt" {
		t.Errorf("child diff files = %q", files)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveFanInTaskNeedsOtherDependenciesLanded(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	// c depends on a and b; its deepest parent is b (the last listed on a tie).
	r := run(t, root, nil, "review", "approve", "g1/c")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g1/c: queued, will publish when the other dependencies land") {
		t.Fatalf("approve with nothing landed: %+v", r)
	}
	if r := run(t, root, nil, "review", "approve", "g1/b"); r.code != ExitOK {
		t.Fatalf("approve b: %+v", r)
	}
	if r := run(t, root, nil, "review", "approve", "g1/a"); r.code != ExitOK {
		t.Fatalf("approve a: %+v", r)
	}
	r = run(t, root, nil, "review", "approve", "g1/c")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g1/c: queued, will publish when the other dependencies land") {
		t.Fatalf("approve with a published, not landed: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/c"]; ok {
		t.Fatal("refused approve pushed a branch")
	}

	setStatus(t, root, "g1/a", store.TaskLanded)
	if r := run(t, root, nil, "review", "approve", "g1/c"); r.code != ExitOK {
		t.Fatalf("approve c after a landed: %+v", r)
	}
	refs := remoteRefs(t, bare)
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", refs["refs/heads/review/g1/c"]); got != refs["refs/heads/review/g1/c"]+" "+refs["refs/heads/review/g1/b"] {
		t.Errorf("c parents = %q, want b's published commit", got)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveRerunsGateWhenHeadIsNotGated(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "g1/a")
	br.GateSHA = ""
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Gate re-run at the head: passed") {
		t.Fatalf("approve: %+v", r)
	}
	after, _ := s.GetBranch(ctx, "g1/a")
	if after.GateSHA != after.HeadSHA || after.PublishedSHA == "" {
		t.Errorf("branch = %+v", after)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveRefusesWhenGateRerunFails(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "g1/a")
	br.GateSHA = ""
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	cfgPath := project.PathsAt(root).Config
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), `check_cmd = "true"`, `check_cmd = "exit 7"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "the gate must be green at the current head") ||
		!strings.Contains(r.stderr, "exit 7 (exit 7") || strings.Contains(r.stderr, "test -f a.txt") {
		t.Fatalf("approve: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; ok {
		t.Error("failing gate pushed a branch")
	}
	tk, _ := s.GetTask(ctx, "g1/a")
	after, _ := s.GetBranch(ctx, "g1/a")
	if tk.Status != store.TaskVerified || after.GateSHA != "" || after.PublishedSHA != "" {
		t.Errorf("task %s, branch %+v", tk.Status, after)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

func TestApproveRefusesConflictWithBase(t *testing.T) {
	root, bare := publishedGoal(t)
	commitOnMain(t, root, "a.txt", "other\n")
	gitC(t, root, "push", "-q", "origin", "main")
	before := remoteRefs(t, bare)

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "no conflict with the base") || !strings.Contains(r.stderr, "a.txt") ||
		!strings.Contains(r.stderr, "origin/main") || !strings.Contains(r.stderr, "mill task rebase g1/a") {
		t.Fatalf("approve: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; ok {
		t.Error("conflicting task pushed a branch")
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a"); tk.Status != store.TaskVerified {
		t.Errorf("status = %s", tk.Status)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
}

// commitInWorktree adds a commit to the task's branch and records it as the head, as a new
// pipeline attempt would.
func commitInWorktree(t *testing.T, s *store.Store, id, name, body string) store.Branch {
	t.Helper()
	ctx := context.Background()
	br, err := s.GetBranch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(br.Worktree, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, br.Worktree, "add", name)
	gitC(t, br.Worktree, "commit", "-q", "-m", "more "+name)
	br.HeadSHA = gitC(t, br.Worktree, "rev-parse", "HEAD")
	br.GateSHA = br.HeadSHA
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	return br
}

func TestRepublishForcePushesWithLease(t *testing.T) {
	root, bare := publishedGoal(t)
	before := remoteRefs(t, bare)
	ctx := context.Background()
	s := openStore(t, root)
	if r := run(t, root, nil, "review", "approve", "g1/a"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	first := remoteRefs(t, bare)["refs/heads/review/g1/a"]

	// Nothing new: refused.
	if r := run(t, root, nil, "review", "approve", "g1/a"); r.code != ExitError || !strings.Contains(r.stderr, "no new changes") {
		t.Fatalf("approve without changes: %+v", r)
	}

	br := commitInWorktree(t, s, "g1/a", "more.txt", "m\n")
	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Republished g1/a") {
		t.Fatalf("republish: %+v", r)
	}
	second := remoteRefs(t, bare)["refs/heads/review/g1/a"]
	if second == first {
		t.Fatal("republish did not move the remote branch")
	}
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", second); got != second+" "+br.BaseSHA {
		t.Errorf("republished parents = %q, want only the base %s", got, br.BaseSHA)
	}
	if got, want := gitC(t, bare, "rev-parse", second+"^{tree}"), gitC(t, root, "rev-parse", br.HeadSHA+"^{tree}"); got != want {
		t.Errorf("republished tree = %s, want %s", got, want)
	}
	if after, _ := s.GetBranch(ctx, "g1/a"); after.PublishedSHA != second {
		t.Errorf("published_sha = %s, want %s", after.PublishedSHA, second)
	}

	// Someone else moves the remote branch: the lease is stale and the push is rejected.
	gitC(t, root, "push", "-q", "--force", "origin", first+":refs/heads/review/g1/a")
	commitInWorktree(t, s, "g1/a", "again.txt", "x\n")
	r = run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "it must still be at "+second[:8]) {
		t.Fatalf("stale lease: %+v", r)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g1/a"]; got != first {
		t.Errorf("stale-lease push moved the remote branch to %s", got)
	}
	if after, _ := s.GetBranch(ctx, "g1/a"); after.PublishedSHA != second {
		t.Errorf("stale-lease push changed published_sha to %s", after.PublishedSHA)
	}
	assertOnlyReviewRefsMoved(t, bare, before)
	for name := range remoteRefs(t, bare) {
		if name != "refs/heads/main" && name != "refs/heads/review/g1/a" {
			t.Errorf("unexpected remote ref %s", name)
		}
	}
}

func TestApproveRefusesWithoutConfiguredRemote(t *testing.T) {
	root := verifiedGoal(t)
	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, `remote "origin" is not configured in git`) ||
		!strings.Contains(r.stderr, "git remote add origin <url>") {
		t.Fatalf("approve: %+v", r)
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a"); tk.Status != store.TaskVerified {
		t.Errorf("status = %s", tk.Status)
	}
}

func TestApproveRefusesUnverifiedTask(t *testing.T) {
	root, bare := publishedGoal(t)
	setStatus(t, root, "g1/a", store.TaskBlocked)
	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "the task must be verified") || !strings.Contains(r.stderr, "g1/a is blocked") {
		t.Fatalf("approve blocked: %+v", r)
	}
	if r := run(t, root, nil, "review", "approve"); r.code != ExitUsage {
		t.Errorf("approve without id: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; ok {
		t.Error("refused approve pushed a branch")
	}
	if !gitx.BranchExists(context.Background(), root, "mill/g1/a") {
		t.Error("task branch disappeared")
	}
	assertNoReviewClaims(t, root)
}

func TestApprovePublishesWhileUnrelatedRunHoldsLock(t *testing.T) {
	root, bare := publishedGoal(t)
	s := openStore(t, root)
	holdRunLock(t, root)
	otherID, _ := runningAttempt(t, s, "g1/b")
	w := startWorker(t, s, otherID)

	r := run(t, root, nil, "review", "approve", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Published g1/a") {
		t.Fatalf("approve while locked: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/a"]; !ok {
		t.Error("approve did not push the review branch")
	}
	if tk, _ := s.GetTask(context.Background(), "g1/a"); tk.Status != store.TaskPublished {
		t.Errorf("task = %+v", tk)
	}
	assertNoReviewClaims(t, root)
	assertWorkerUntouched(t, s, "g1/b", w)
}

func TestApprovePublishesFreeTasksAndReportsBusyOnes(t *testing.T) {
	root, bare := publishedGoal(t)
	s := openStore(t, root)
	bID, n := runningAttempt(t, s, "g1/b")
	w := startWorker(t, s, bID)
	before := snapshotTask(t, s, "g1/b")

	r := run(t, root, nil, "review", "approve", "g1/a", "g1/b")
	busy := fmt.Sprintf("mill review approve: task g1/b is busy: attempt %d (verifier) running", n)
	if r.code != ExitError || !strings.Contains(r.stdout, "Published g1/a") || !strings.Contains(r.stderr, busy) {
		t.Fatalf("approve: %+v", r)
	}
	refs := remoteRefs(t, bare)
	if _, ok := refs["refs/heads/review/g1/a"]; !ok {
		t.Error("the free task was not published")
	}
	if _, ok := refs["refs/heads/review/g1/b"]; ok {
		t.Error("the busy task was pushed")
	}
	if after := snapshotTask(t, s, "g1/b"); !reflect.DeepEqual(after, before) {
		t.Errorf("busy task changed:\nbefore %+v\nafter  %+v", before, after)
	}
	assertNoReviewClaims(t, root)
	assertWorkerUntouched(t, s, "g1/b", w)

	r = run(t, root, nil, "review", "approve", "--goal", "g1")
	if r.code != ExitError || !strings.Contains(r.stderr, busy) {
		t.Fatalf("approve --goal: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g1/b"]; ok {
		t.Error("approve --goal pushed the busy task")
	}
	assertNoReviewClaims(t, root)
	assertWorkerUntouched(t, s, "g1/b", w)
}

func TestApproveChildOfLandedParentNeedsSyncFirst(t *testing.T) {
	root, bare := chainPublished(t, false)
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("approve parent: %+v", r)
	}
	// The forge squash-merges the parent: its branch commits are not in main.
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--squash", "origin/review/g2/a")
	gitC(t, f, "commit", "-q", "-m", "A (squashed)")
	squash := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")
	setStatus(t, root, "g2/a", store.TaskLanded)

	r := run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g2/b: queued, will publish when `mill sync` moves it onto the base branch") {
		t.Fatalf("approve before sync: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Fatal("refused approve pushed a branch")
	}

	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "g2/b  onto main ("+squash[:8]) {
		t.Fatalf("sync: %+v", r)
	}
	if r := run(t, root, nil, "review", "approve", "g2/b"); r.code != ExitOK {
		t.Fatalf("approve after sync: %+v", r)
	}
	pub := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", pub); got != pub+" "+squash {
		t.Errorf("child parents = %q, want main's tip %s", got, squash)
	}
	if files := gitC(t, bare, "diff", "--name-only", squash, pub); files != "b.txt" {
		t.Errorf("child diff files = %q", files)
	}
}

func TestApproveChildRefusedWhileParentHasUnpublishedChanges(t *testing.T) {
	root, bare := chainPublished(t, false)
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("approve parent: %+v", r)
	}
	commitInWorktree(t, openStore(t, root), "g2/a", "more.txt", "m\n")

	r := run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g2/b: queued, will publish when parent g2/a publishes its new changes") {
		t.Fatalf("approve child: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Fatal("refused approve pushed a branch")
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g2/b"); tk.Status != store.TaskVerified {
		t.Errorf("g2/b status = %s", tk.Status)
	}

	// Once the parent is published again and sync restacked the child, it publishes on top.
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("republish parent: %+v", r)
	}
	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "g2/b  onto mill/g2/a") {
		t.Fatalf("sync: %+v", r)
	}
	if r := run(t, root, nil, "review", "approve", "g2/b"); r.code != ExitOK {
		t.Fatalf("approve child after parent: %+v", r)
	}
	refs := remoteRefs(t, bare)
	parentPub, childPub := refs["refs/heads/review/g2/a"], refs["refs/heads/review/g2/b"]
	if files := gitC(t, bare, "diff", "--name-only", parentPub, childPub); files != "b.txt" {
		t.Errorf("child diff files = %q", files)
	}
}
