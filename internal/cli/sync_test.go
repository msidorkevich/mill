package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/store"
)

// chainPublished runs the chain plan as g2 (b depends on a), adds a bare remote and, when
// publish is set, publishes both tasks.
func chainPublished(t *testing.T, publish bool) (root, bare string) {
	t.Helper()
	root = goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, chainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve plan: %+v", r)
	}
	if r := run(t, root, nil, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	bare = addRemote(t, root)
	if publish {
		for _, id := range []string{"g2/a", "g2/b"} {
			if r := run(t, root, nil, "review", "approve", id); r.code != ExitOK {
				t.Fatalf("approve %s: %+v", id, r)
			}
		}
	}
	return root, bare
}

// forge is a clone of the bare remote on main where tests play the forge's merge button.
func forge(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "forge")
	gitC(t, t.TempDir(), "clone", "-q", bare, dir)
	gitC(t, dir, "checkout", "-q", "-B", "main", "origin/main")
	return dir
}

func forgeCommit(t *testing.T, dir, name, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, dir, "add", name)
	gitC(t, dir, "commit", "-q", "-m", msg)
}

func lastEvent(t *testing.T, s *store.Store, id, kind string) map[string]any {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == kind {
			var d map[string]any
			if err := json.Unmarshal(evs[i].Data, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("%s has no %s event", id, kind)
	return nil
}

func TestSyncMergeCommitLandsTaskAndRestacksChildOntoBase(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	oldChildPub := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	oldChild, _ := s.GetBranch(ctx, "g2/b")

	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	merge := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	for _, want := range []string{"g2/a: landed in " + merge[:8] + " (ancestor)", "Landed:", "g2/a  ancestor in " + merge[:8],
		"Restacked:", "g2/b  onto main (" + merge[:8], "Republished:", "g2/b  review/g2/b"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if tk, _ := s.GetTask(ctx, "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s", tk.Status)
	}
	if ev := lastEvent(t, s, "g2/a", "task-landed"); ev["method"] != "ancestor" || ev["commit"] != merge {
		t.Errorf("task-landed = %v", ev)
	}

	// The child is rebased onto the remote base, re-gated and re-published with the lease.
	child, _ := s.GetBranch(ctx, "g2/b")
	if child.Base != "main" || child.BaseSHA != merge || child.StartSHA != merge || child.GateSHA != child.HeadSHA || child.HeadSHA == oldChild.HeadSHA {
		t.Errorf("child branch = %+v", child)
	}
	if got := gitC(t, root, "rev-parse", "mill/g2/b"); got != child.HeadSHA {
		t.Errorf("git branch at %s, recorded %s", got, child.HeadSHA)
	}
	if files := gitC(t, root, "diff", "--name-only", merge, child.HeadSHA); files != "b.txt" {
		t.Errorf("child's own changes = %q", files)
	}
	newPub := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	if newPub == oldChildPub || newPub != child.PublishedSHA {
		t.Errorf("review/g2/b = %s (was %s), recorded %s", newPub, oldChildPub, child.PublishedSHA)
	}
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", newPub); got != newPub+" "+merge {
		t.Errorf("republished parents = %q, want %s", got, merge)
	}
	if tk, _ := s.GetTask(ctx, "g2/b"); tk.Status != store.TaskPublished {
		t.Errorf("g2/b status = %s", tk.Status)
	}
	if ev := lastEvent(t, s, "g2/b", "task-gate"); ev == nil {
		t.Error("no gate re-run recorded")
	}
	if g, _ := s.GetGoal(ctx, "g2"); g.Status == store.GoalLanded {
		t.Error("goal landed with a task still published")
	}

	// The child merges too: every task has landed, so the goal has.
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/b", "origin/review/g2/b")
	gitC(t, f, "push", "-q", "origin", "main")
	r = run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/b: landed") || !strings.Contains(r.stdout, "Goals landed: g2") {
		t.Fatalf("second sync: %+v", r)
	}
	if g, _ := s.GetGoal(ctx, "g2"); g.Status != store.GoalLanded {
		t.Errorf("goal status = %s", g.Status)
	}
	if l := run(t, root, nil, "goal", "list"); !strings.Contains(l.stdout, "landed") {
		t.Errorf("goal list:\n%s", l.stdout)
	}
	if sh := run(t, root, nil, "goal", "show", "g2"); !strings.Contains(sh.stdout, "landed") {
		t.Errorf("goal show:\n%s", sh.stdout)
	}
	// A further sync changes nothing.
	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing changed.") {
		t.Errorf("idle sync: %+v", r)
	}
}

func TestSyncSquashMergeLandsTaskByPatchAndPrintsJSON(t *testing.T) {
	root, bare := chainPublished(t, true)
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--squash", "origin/review/g2/a")
	gitC(t, f, "commit", "-q", "-m", "A (squashed)")
	squash := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync", "--json")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	var doc struct {
		Schema string `json:"schema"`
		Landed []struct {
			TaskID string `json:"task_id"`
			Method string `json:"method"`
			Commit string `json:"commit"`
		} `json:"landed"`
		Restacked []struct {
			TaskID string `json:"task_id"`
		} `json:"restacked"`
		Republished []struct {
			TaskID string `json:"task_id"`
		} `json:"republished"`
		Blocked []json.RawMessage `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	if doc.Schema != "mill/sync@1" || len(doc.Landed) != 1 || doc.Landed[0].TaskID != "g2/a" || doc.Landed[0].Method != "patch" ||
		doc.Landed[0].Commit != squash || len(doc.Restacked) != 1 || len(doc.Republished) != 1 || doc.Blocked == nil || len(doc.Blocked) != 0 {
		t.Errorf("doc = %+v", doc)
	}
	if ev := lastEvent(t, openStore(t, root), "g2/a", "task-landed"); ev["method"] != "patch" || ev["commit"] != squash {
		t.Errorf("task-landed = %v", ev)
	}
}

func TestSyncChangedPatchWithTrailerLandsTaskByTrailer(t *testing.T) {
	root, bare := chainPublished(t, true)
	f := forge(t, bare)
	forgeCommit(t, f, "a.txt", "a, edited on merge\n", "A, edited\n\nMill-Task: g2/a")
	edited := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/a  trailer in "+edited[:8]) {
		t.Fatalf("sync: %+v", r)
	}
	s := openStore(t, root)
	if ev := lastEvent(t, s, "g2/a", "task-landed"); ev["method"] != "trailer" || ev["commit"] != edited {
		t.Errorf("task-landed = %v", ev)
	}
	if tk, _ := s.GetTask(context.Background(), "g2/b"); tk.Status != store.TaskPublished {
		t.Errorf("g2/b status = %s", tk.Status)
	}
}

func TestSyncRestacksChildOntoParentsNewHead(t *testing.T) {
	root, _ := chainPublished(t, false)
	ctx := context.Background()
	s := openStore(t, root)
	old, _ := s.GetBranch(ctx, "g2/b")
	parent := commitInWorktree(t, s, "g2/a", "more.txt", "m\n")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/b  onto mill/g2/a ("+parent.HeadSHA[:8]) || strings.Contains(r.stdout, "Republished") {
		t.Fatalf("sync: %+v", r)
	}
	child, _ := s.GetBranch(ctx, "g2/b")
	if child.Base != "mill/g2/a" || child.BaseSHA != parent.HeadSHA || child.StartSHA != parent.HeadSHA ||
		child.HeadSHA == old.HeadSHA || child.GateSHA != child.HeadSHA {
		t.Errorf("child branch = %+v", child)
	}
	if files := gitC(t, root, "diff", "--name-only", parent.HeadSHA, "mill/g2/b"); files != "b.txt" {
		t.Errorf("child's own changes = %q", files)
	}
	if tk, _ := s.GetTask(ctx, "g2/b"); tk.Status != store.TaskVerified {
		t.Errorf("g2/b status = %s", tk.Status)
	}
	if ev := lastEvent(t, s, "g2/b", "task-restacked"); ev["new_head"] != child.HeadSHA {
		t.Errorf("task-restacked = %v", ev)
	}
}

func TestSyncConflictingRestackBlocksChildAndKeepsBranch(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "g2/b")
	pubBefore := remoteRefs(t, bare)["refs/heads/review/g2/b"]

	f := forge(t, bare)
	forgeCommit(t, f, "b.txt", "someone else's b\n", "main: b.txt")
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Blocked:") || !strings.Contains(r.stdout, "conflicts in b.txt") {
		t.Fatalf("sync: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g2/b")
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "b.txt") {
		t.Errorf("g2/b = %s %q", tk.Status, tk.Reason)
	}
	after, _ := s.GetBranch(ctx, "g2/b")
	if after.HeadSHA != before.HeadSHA || after.Base != before.Base || after.BaseSHA != before.BaseSHA {
		t.Errorf("branch row changed: %+v -> %+v", before, after)
	}
	if got := gitC(t, root, "rev-parse", "mill/g2/b"); got != before.HeadSHA {
		t.Errorf("branch moved to %s, want %s", got, before.HeadSHA)
	}
	if dirty := gitC(t, before.Worktree, "status", "--porcelain"); dirty != "" {
		t.Errorf("worktree left dirty:\n%s", dirty)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/b"]; got != pubBefore {
		t.Error("blocked task was re-published")
	}
}

func TestSyncRedGateAfterRestackBlocksChild(t *testing.T) {
	root, bare := chainPublished(t, true)
	pubBefore := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	gitC(t, f, "push", "-q", "origin", "main")

	cfgPath := project.PathsAt(root).Config
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), `check_cmd = "true"`, `check_cmd = "exit 7"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Restacked:") || !strings.Contains(r.stdout, "gate failed after restacking") {
		t.Fatalf("sync: %+v", r)
	}
	s := openStore(t, root)
	tk, _ := s.GetTask(context.Background(), "g2/b")
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "exit 7") || strings.Contains(tk.Reason, "test -f b.txt") {
		t.Errorf("g2/b = %s %q", tk.Status, tk.Reason)
	}
	br, _ := s.GetBranch(context.Background(), "g2/b")
	if br.GateSHA == br.HeadSHA {
		t.Error("gate_sha recorded for a red gate")
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/b"]; got != pubBefore {
		t.Error("task with a red gate was re-published")
	}
}

func TestSyncRestacksFanInTaskWhenDependencyLanded(t *testing.T) {
	root, bare := publishedGoal(t)
	for _, id := range []string{"g1/a", "g1/b"} {
		if r := run(t, root, nil, "review", "approve", id); r.code != ExitOK {
			t.Fatalf("approve %s: %+v", id, r)
		}
	}
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "g1/c")
	b, _ := s.GetBranch(ctx, "g1/b")
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g1/a", "origin/review/g1/a")
	merge := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g1/a  ancestor") || !strings.Contains(r.stdout, "Restacked:") ||
		!strings.Contains(r.stdout, "g1/c  onto mill/g1/b") || strings.Contains(r.stdout, "Not restacked") {
		t.Fatalf("sync: %+v", r)
	}
	// c stays on its deepest dependency b; a's changes now come from the base branch, so the
	// landed a is not merged again.
	after, _ := s.GetBranch(ctx, "g1/c")
	if after.Base != "mill/g1/b" || after.BaseSHA != b.HeadSHA || after.HeadSHA == before.HeadSHA || after.GateSHA != after.HeadSHA {
		t.Fatalf("fan-in branch = %+v (before %+v)", after, before)
	}
	if got := gitC(t, root, "rev-list", "--parents", "-n1", after.StartSHA); got != after.StartSHA+" "+b.HeadSHA+" "+merge {
		t.Errorf("start parents = %q, want b head %s and the base tip %s", got, b.HeadSHA, merge)
	}
	if in := gitC(t, root, "merge-base", "--is-ancestor", merge, after.HeadSHA); in != "" {
		t.Errorf("merge-base output %q", in)
	}
	if files := gitC(t, root, "diff", "--name-only", after.StartSHA, after.HeadSHA); files != "c.txt" {
		t.Errorf("own changes = %q", files)
	}
	if tk, _ := s.GetTask(ctx, "g1/c"); tk.Status != store.TaskVerified {
		t.Errorf("g1/c status = %s", tk.Status)
	}
	if ev := lastEvent(t, s, "g1/c", "task-restacked"); ev["new_start"] != after.StartSHA {
		t.Errorf("task-restacked = %v", ev)
	}

	// Nothing moved since: a second sync leaves it alone.
	if r := run(t, root, nil, "sync"); r.code != ExitOK || strings.Contains(r.stdout, "Restacked:") {
		t.Fatalf("second sync: %+v", r)
	}
}

func TestSyncBlocksFanInTaskOnRestackConflict(t *testing.T) {
	root, _ := publishedGoal(t)
	ctx := context.Background()
	s := openStore(t, root)
	b, _ := s.GetBranch(ctx, "g1/b")
	before, _ := s.GetBranch(ctx, "g1/c")
	// b moves with a commit that writes c.txt differently from c's own commit.
	if err := os.WriteFile(filepath.Join(b.Worktree, "c.txt"), []byte("from b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, b.Worktree, "add", "c.txt")
	gitC(t, b.Worktree, "commit", "-q", "-m", "b writes c.txt")
	b.HeadSHA = gitC(t, b.Worktree, "rev-parse", "HEAD")
	if err := s.SaveBranch(ctx, b); err != nil {
		t.Fatal(err)
	}

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Blocked:") || strings.Contains(r.stdout, "Restacked:") {
		t.Fatalf("sync: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g1/c")
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "c.txt") || !strings.Contains(tk.Reason, "branch is unchanged") {
		t.Errorf("g1/c = %s %q", tk.Status, tk.Reason)
	}
	after, _ := s.GetBranch(ctx, "g1/c")
	if after != before {
		t.Errorf("fan-in branch changed: %+v -> %+v", before, after)
	}
	if got := gitC(t, after.Worktree, "rev-parse", "HEAD"); got != before.HeadSHA {
		t.Errorf("head = %s, want %s", got, before.HeadSHA)
	}
	if got := gitC(t, after.Worktree, "status", "--porcelain"); got != "" {
		t.Errorf("worktree not clean: %q", got)
	}
}

func TestSyncNeedsConfiguredRemote(t *testing.T) {
	root := verifiedGoal(t)
	r := run(t, root, nil, "sync")
	if r.code != ExitError || !strings.Contains(r.stderr, `remote "origin" is not configured in git`) {
		t.Fatalf("sync: %+v", r)
	}
	if r := run(t, root, nil, "sync", "extra"); r.code != ExitUsage {
		t.Errorf("sync with argument: %+v", r)
	}
}

func TestSyncLeavesChildPublishedWhenParentHasUnpublishedChanges(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	pubBefore := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	parent := commitInWorktree(t, s, "g2/a", "more.txt", "m\n")

	r := run(t, root, nil, "sync", "--json")
	if r.code != ExitOK {
		t.Fatalf("sync: %+v", r)
	}
	var doc struct {
		Restacked []struct {
			TaskID string `json:"task_id"`
		} `json:"restacked"`
		Republished    []json.RawMessage `json:"republished"`
		NeedsRepublish []struct {
			TaskID string `json:"task_id"`
			After  string `json:"after"`
			Reason string `json:"reason"`
		} `json:"needs_republish"`
		Blocked []json.RawMessage `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	if len(doc.Restacked) != 1 || doc.Restacked[0].TaskID != "g2/b" || len(doc.Republished) != 0 || len(doc.Blocked) != 0 ||
		len(doc.NeedsRepublish) != 1 || doc.NeedsRepublish[0].TaskID != "g2/b" || doc.NeedsRepublish[0].After != "g2/a" ||
		!strings.Contains(doc.NeedsRepublish[0].Reason, "mill review approve g2/a") {
		t.Fatalf("doc = %+v", doc)
	}
	tk, _ := s.GetTask(ctx, "g2/b")
	child, _ := s.GetBranch(ctx, "g2/b")
	if tk.Status != store.TaskPublished || child.BaseSHA != parent.HeadSHA || child.PublishedSHA != pubBefore {
		t.Errorf("g2/b = %s, branch %+v", tk.Status, child)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/b"]; got != pubBefore {
		t.Error("child was re-published on top of the parent's unpublished changes")
	}
	if ev := lastEvent(t, s, "g2/b", "task-restacked"); ev["new_head"] != child.HeadSHA {
		t.Errorf("task-restacked = %v", ev)
	}

	// The text summary says what to do; it stays pending until the parent is published.
	r = run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Needs re-publishing after its parent") ||
		!strings.Contains(r.stdout, "g2/b  after g2/a: run `mill review approve g2/a`, then `mill sync`") || strings.Contains(r.stdout, "Blocked:") {
		t.Fatalf("second sync: %+v", r)
	}

	// Publishing the parent and syncing re-publishes the child on top of it.
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("republish parent: %+v", r)
	}
	r = run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Republished:") || !strings.Contains(r.stdout, "g2/b  review/g2/b") {
		t.Fatalf("sync after parent: %+v", r)
	}
	refs := remoteRefs(t, bare)
	parentPub, childPub := refs["refs/heads/review/g2/a"], refs["refs/heads/review/g2/b"]
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", childPub); got != childPub+" "+parentPub {
		t.Errorf("child parents = %q, want %s", got, parentPub)
	}
	if files := gitC(t, bare, "diff", "--name-only", parentPub, childPub); files != "b.txt" {
		t.Errorf("child diff files = %q", files)
	}
	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing changed.") {
		t.Errorf("idle sync: %+v", r)
	}
}

func TestSyncLandsVerifiedTaskWhoseBranchWasMergedDirectly(t *testing.T) {
	root, bare := chainPublished(t, false)
	ctx := context.Background()
	gitC(t, root, "push", "-q", "origin", "mill/g2/a:refs/heads/feature")
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge feature", "origin/feature")
	merge := gitC(t, f, "rev-parse", "HEAD")
	gitC(t, f, "push", "-q", "origin", "main")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Landed:") || !strings.Contains(r.stdout, "g2/a  merged in "+merge[:8]) ||
		!strings.Contains(r.stdout, "g2/b  onto main ("+merge[:8]) {
		t.Fatalf("sync: %+v", r)
	}
	s := openStore(t, root)
	if tk, _ := s.GetTask(ctx, "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s", tk.Status)
	}
	if ev := lastEvent(t, s, "g2/a", "task-landed"); ev["method"] != "merged" || ev["commit"] != merge {
		t.Errorf("task-landed = %v", ev)
	}
	if tk, _ := s.GetTask(ctx, "g2/b"); tk.Status != store.TaskVerified {
		t.Errorf("g2/b status = %s", tk.Status)
	}
}

// busyWorker is a running attempt on a task whose recorded worker is a live process.
type busyWorker struct {
	s       *store.Store
	attempt int64
	pid     int
	doing   string
	exited  chan struct{}
}

// startBusyAttempt records a running reviewer attempt on id whose worker pid is a live
// `sleep` process, as a `mill run` in another terminal would.
func startBusyAttempt(t *testing.T, root, id string) *busyWorker {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, root)
	n, err := s.MaxAttemptN(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	aid, err := s.StartAttempt(ctx, id, n+1, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &busyWorker{s: s, attempt: aid, pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() { cmd.Wait(); close(w.exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-w.exited })
	if err := s.SetAttemptProcess(ctx, aid, w.pid, "claude", ""); err != nil {
		t.Fatal(err)
	}
	w.doing = "attempt " + strconv.Itoa(n+1) + " (reviewer) running"
	return w
}

// assertUntouched checks the attempt is still running with its pid and the worker is alive.
func (w *busyWorker) assertUntouched(t *testing.T) {
	t.Helper()
	select {
	case <-w.exited:
		t.Errorf("worker pid %d was killed", w.pid)
	default:
	}
	running, err := w.s.RunningAttempts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range running {
		if a.ID == w.attempt && a.PID == w.pid {
			return
		}
	}
	t.Errorf("attempt %d is no longer running with pid %d: %+v", w.attempt, w.pid, running)
}

func assertNoClaims(t *testing.T, root string) {
	t.Helper()
	claims, err := openStore(t, root).ClaimedTasks(context.Background())
	if err != nil || len(claims) != 0 {
		t.Errorf("claims left = %+v, %v", claims, err)
	}
}

func TestSyncLandsMergedTaskWhileRunLockIsHeld(t *testing.T) {
	root, bare := chainPublished(t, true)
	lock, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	f := forge(t, bare)
	mergeReview(t, f, "g2/a")

	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Landed:") {
		t.Fatalf("sync with held lock: %+v", r)
	}
	if tk, _ := openStore(t, root).GetTask(context.Background(), "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s, want landed", tk.Status)
	}
	assertNoClaims(t, root)
}

func TestSyncLeavesBusyTaskUnchangedAndExitsOne(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	oldChild, _ := s.GetBranch(ctx, "g2/b")
	oldPub := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	w := startBusyAttempt(t, root, "g2/b")
	f := forge(t, bare)
	mergeReview(t, f, "g2/a")

	r := run(t, root, nil, "sync")
	if r.code != ExitError {
		t.Fatalf("sync: %+v", r)
	}
	for _, want := range []string{"Landed:", "g2/a  ", "Busy (being worked on; run `mill sync` again later):\n  g2/b  " + w.doing} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "Restacked:") {
		t.Errorf("busy task restacked:\n%s", r.stdout)
	}
	if tk, _ := s.GetTask(ctx, "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s, want landed", tk.Status)
	}
	if tk, _ := s.GetTask(ctx, "g2/b"); tk.Status != store.TaskPublished {
		t.Errorf("g2/b status = %s, want published", tk.Status)
	}
	if got, _ := s.GetBranch(ctx, "g2/b"); got != oldChild {
		t.Errorf("busy g2/b branch row changed: %+v, was %+v", got, oldChild)
	}
	if head := gitC(t, root, "rev-parse", oldChild.Name); head != oldChild.HeadSHA {
		t.Errorf("busy g2/b git branch moved to %s", head)
	}
	if pub := remoteRefs(t, bare)["refs/heads/review/g2/b"]; pub != oldPub {
		t.Errorf("busy g2/b re-published: %s, was %s", pub, oldPub)
	}
	w.assertUntouched(t)

	r = run(t, root, nil, "sync", "--json")
	var doc struct {
		Busy []struct {
			TaskID string `json:"task_id"`
			Doing  string `json:"doing"`
		} `json:"busy"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("sync --json: %v\n%+v", err, r)
	}
	if r.code != ExitError || len(doc.Busy) != 1 || doc.Busy[0].TaskID != "g2/b" || doc.Busy[0].Doing != w.doing {
		t.Fatalf("sync --json: %+v", r)
	}
	w.assertUntouched(t)
	assertNoClaims(t, root)
}

func TestSyncRetriesRestackBlockedPublishedTaskAndRepublishesItOnceConflictIsGone(t *testing.T) {
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	pubBefore := remoteRefs(t, bare)["refs/heads/review/g2/b"]

	f := forge(t, bare)
	forgeCommit(t, f, "b.txt", "someone else's b\n", "main: b.txt")
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	gitC(t, f, "push", "-q", "origin", "main")
	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "conflicts in b.txt") {
		t.Fatalf("first sync: %+v", r)
	}
	if tk, _ := s.GetTask(ctx, "g2/b"); tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskPublished {
		t.Fatalf("g2/b = %s, blocked from %q", tk.Status, tk.RestackBlockedFrom)
	}

	// Still conflicting: the next sync retries and keeps it blocked with the current reason.
	forgeCommit(t, f, "b.txt", "someone else's b, again\n", "main: b.txt again")
	gitC(t, f, "push", "-q", "origin", "main")
	tip := gitC(t, f, "rev-parse", "HEAD")
	r := run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Blocked:") {
		t.Fatalf("second sync: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g2/b")
	if tk.Status != store.TaskBlocked || tk.RestackBlockedFrom != store.TaskPublished || !strings.Contains(tk.Reason, tip[:8]) {
		t.Fatalf("g2/b = %s from %q, reason %q (want the new tip %s)", tk.Status, tk.RestackBlockedFrom, tk.Reason, tip[:8])
	}

	// The cause goes away: main drops its b.txt.
	gitC(t, f, "rm", "-q", "b.txt")
	gitC(t, f, "commit", "-q", "-m", "main: drop b.txt")
	gitC(t, f, "push", "-q", "origin", "main")
	attempts, _ := s.MaxAttemptN(ctx, "g2/b")
	r = run(t, root, nil, "sync")
	if r.code != ExitOK || !strings.Contains(r.stdout, "g2/b: restack succeeded; published again") || !strings.Contains(r.stdout, "Republished") {
		t.Fatalf("third sync: %+v", r)
	}
	tk, _ = s.GetTask(ctx, "g2/b")
	if tk.Status != store.TaskPublished || tk.Reason != "" || tk.RestackBlockedFrom != "" {
		t.Fatalf("g2/b = %s %q from %q", tk.Status, tk.Reason, tk.RestackBlockedFrom)
	}
	if n, _ := s.MaxAttemptN(ctx, "g2/b"); n != attempts {
		t.Errorf("attempts = %d, want %d: no agent may run", n, attempts)
	}
	br, _ := s.GetBranch(ctx, "g2/b")
	if br.GateSHA != br.HeadSHA || br.Base != "main" {
		t.Errorf("branch = %+v", br)
	}
	if got := remoteRefs(t, bare)["refs/heads/review/g2/b"]; got == pubBefore || got != br.PublishedSHA {
		t.Errorf("review/g2/b = %s (was %s), recorded %s", got, pubBefore, br.PublishedSHA)
	}
}

func TestSyncLeavesTaskBlockedForOtherReasonUntouched(t *testing.T) {
	root, _ := chainPublished(t, false)
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "g2/b")
	commitInWorktree(t, s, "g2/a", "more.txt", "m\n")
	if err := s.SetTaskStatus(ctx, "g2/b", store.TaskBlocked, "", "usage limit reached"); err != nil {
		t.Fatal(err)
	}

	if r := run(t, root, nil, "sync"); r.code != ExitOK || strings.Contains(r.stdout, "g2/b") {
		t.Fatalf("sync: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g2/b")
	if tk.Status != store.TaskBlocked || tk.Reason != "usage limit reached" || tk.RestackBlockedFrom != "" {
		t.Errorf("g2/b = %s %q from %q", tk.Status, tk.Reason, tk.RestackBlockedFrom)
	}
	if after, _ := s.GetBranch(ctx, "g2/b"); after.HeadSHA != before.HeadSHA || after.BaseSHA != before.BaseSHA {
		t.Errorf("branch changed: %+v -> %+v", before, after)
	}
}

func TestSyncRetriesTaskBlockedByOlderRestackConflictAndRestoresIt(t *testing.T) {
	root, _ := chainPublished(t, false)
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "g2/b")
	commitInWorktree(t, s, "g2/a", "more.txt", "m\n")
	// Blocked the way sync did before it recorded the former status: no restack_from.
	if err := s.SetTaskStatus(ctx, "g2/b", store.TaskBlocked, "",
		"sync: restacking fan-in task onto origin/main (1234abcd) conflicts at replaying 5678 in b.txt; the branch is unchanged. Restack it by hand in x, then run `mill run --retry-blocked`"); err != nil {
		t.Fatal(err)
	}
	attempts, _ := s.MaxAttemptN(ctx, "g2/b")

	if r := run(t, root, nil, "sync"); r.code != ExitOK || !strings.Contains(r.stdout, "g2/b: restack succeeded; verified again") {
		t.Fatalf("sync: %+v", r)
	}
	tk, _ := s.GetTask(ctx, "g2/b")
	if tk.Status != store.TaskVerified || tk.Reason != "" || tk.RestackBlockedFrom != "" {
		t.Fatalf("g2/b = %s %q from %q", tk.Status, tk.Reason, tk.RestackBlockedFrom)
	}
	if n, _ := s.MaxAttemptN(ctx, "g2/b"); n != attempts {
		t.Errorf("attempts = %d, want %d: no agent may run", n, attempts)
	}
	after, _ := s.GetBranch(ctx, "g2/b")
	if after.BaseSHA == before.BaseSHA || after.GateSHA != after.HeadSHA {
		t.Errorf("branch not restacked and gated: %+v -> %+v", before, after)
	}
}
