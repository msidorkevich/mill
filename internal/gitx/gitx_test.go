package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repo creates a repository with one commit on main and returns its path.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	mustRun(t, ctx, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitAll(ctx, dir, "initial"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustRun(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := Run(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorktreeCommitDiff(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	base, err := RevParse(ctx, r, "main")
	if err != nil || len(base) != 40 {
		t.Fatalf("RevParse = %q, %v", base, err)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(ctx, r, wt, "mill/t/x", base); err != nil {
		t.Fatal(err)
	}
	if !BranchExists(ctx, r, "mill/t/x") || !IsWorktreeOn(ctx, wt, "mill/t/x") {
		t.Fatal("branch/worktree not created")
	}
	if IsWorktreeOn(ctx, wt, "main") {
		t.Fatal("IsWorktreeOn matched the wrong branch")
	}

	if changed, _ := HasChanges(ctx, wt); changed {
		t.Fatal("fresh worktree reports changes")
	}
	if err := os.WriteFile(filepath.Join(wt, "new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := HasChanges(ctx, wt); !changed {
		t.Fatal("untracked file not reported as a change")
	}
	head, err := CommitAll(ctx, wt, "mill: x attempt 1\n\nMill-Task: x\n")
	if err != nil || head == base {
		t.Fatalf("CommitAll = %q, %v", head, err)
	}
	if msg := mustRun(t, ctx, wt, "log", "-1", "--format=%B"); !strings.Contains(msg, "Mill-Task: x") {
		t.Fatalf("commit message = %q", msg)
	}

	diff, err := Diff(ctx, wt, base)
	if err != nil || !strings.Contains(diff, "+package x") {
		t.Fatalf("Diff = %q, %v", diff, err)
	}
	stat, err := DiffStat(ctx, wt, base)
	if err != nil || !strings.Contains(stat, "new.go") {
		t.Fatalf("DiffStat = %q, %v", stat, err)
	}
	// The main checkout is untouched.
	if _, err := os.Stat(filepath.Join(r, "new.go")); err == nil {
		t.Fatal("worktree commit leaked into the main checkout")
	}
}

func TestReattachExistingBranch(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	base, _ := RevParse(ctx, r, "main")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(ctx, r, wt, "mill/t/y", base); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := PruneWorktrees(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := AddWorktreeForBranch(ctx, r, wt, "mill/t/y"); err != nil {
		t.Fatal(err)
	}
	if !IsWorktreeOn(ctx, wt, "mill/t/y") {
		t.Fatal("reattached worktree not on the branch")
	}
}

func TestCommitUsesMillIdentityWhenNoneConfigured(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir()) // no global identity
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r := repo(t)
	if author := mustRun(t, ctx, r, "log", "-1", "--format=%an <%ae>"); author != "mill <mill@localhost>" {
		t.Fatalf("author = %q", author)
	}
}

func TestGitEnvIsIgnored(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	t.Setenv("GIT_DIR", "/nonexistent")
	if _, err := RevParse(ctx, r, "HEAD"); err != nil {
		t.Fatalf("GIT_DIR leaked into git calls: %v", err)
	}
}

func TestErrorsIncludeStderr(t *testing.T) {
	r := repo(t)
	_, err := Run(context.Background(), r, "rev-parse", "--verify", "no-such-ref")
	if err == nil || !strings.Contains(err.Error(), "git rev-parse") {
		t.Fatalf("err = %v", err)
	}
}

func TestMergeCreatesMergeCommitAndReportsConflicts(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, ctx, dir, "checkout", "-q", "-b", "one")
	write("a.txt", "one\n")
	write("shared.txt", "one\n")
	if _, err := CommitAll(ctx, dir, "one"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, dir, "checkout", "-q", "-b", "two", "main")
	write("b.txt", "two\n")
	if _, err := CommitAll(ctx, dir, "two"); err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, dir, "one", "merge one"); err != nil {
		t.Fatal(err)
	}
	if parents := mustRun(t, ctx, dir, "log", "-1", "--format=%P"); len(strings.Fields(parents)) != 2 {
		t.Fatalf("not a merge commit, parents = %q", parents)
	}

	mustRun(t, ctx, dir, "checkout", "-q", "-b", "three", "main")
	write("shared.txt", "three\n")
	if _, err := CommitAll(ctx, dir, "three"); err != nil {
		t.Fatal(err)
	}
	err := Merge(ctx, dir, "one", "merge one")
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Files) != 1 || ce.Files[0] != "shared.txt" {
		t.Fatalf("err = %v", err)
	}
	if _, err := Run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		t.Fatal("merge still in progress")
	}
	if has, _ := HasChanges(ctx, dir); has {
		t.Fatal("worktree dirty after abort")
	}
}

func TestChangedFilesListsAddedModifiedDeletedRenamed(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(r, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("gone.txt", "to delete\n")
	write("old.txt", "some content that is long enough to detect a rename\nline2\nline3\n")
	if _, err := CommitAll(ctx, r, "more"); err != nil {
		t.Fatal(err)
	}
	from, _ := RevParse(ctx, r, "HEAD")
	write("README", "changed\n")
	write("added.txt", "new\n")
	if err := os.Remove(filepath.Join(r, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, r, "mv", "old.txt", "new name.txt")
	if _, err := CommitAll(ctx, r, "change"); err != nil {
		t.Fatal(err)
	}
	got, err := ChangedFiles(ctx, r, from)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"README": true, "added.txt": true, "gone.txt": true, "new name.txt": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, f := range got {
		if !want[f] {
			t.Fatalf("unexpected %q in %v", f, got)
		}
	}
}

func TestDetachedWorktreeAtShaIsAddedAndRemoved(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	first, _ := RevParse(ctx, r, "main")
	if err := os.WriteFile(filepath.Join(r, "later"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitAll(ctx, r, "later"); err != nil {
		t.Fatal(err)
	}
	branches := mustRun(t, ctx, r, "branch", "--list")

	wt := filepath.Join(t.TempDir(), "base")
	if err := AddDetachedWorktree(ctx, r, wt, first); err != nil {
		t.Fatal(err)
	}
	if head, _ := RevParse(ctx, wt, "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want %s", head, first)
	}
	if _, err := os.Stat(filepath.Join(wt, "later")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree is not at the requested commit: %v", err)
	}
	if _, err := Run(ctx, wt, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatal("HEAD is not detached")
	}
	if got := mustRun(t, ctx, r, "branch", "--list"); got != branches {
		t.Fatalf("branches changed: %q -> %q", branches, got)
	}

	if err := RemoveWorktree(ctx, r, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree directory left behind: %v", err)
	}
	if list := mustRun(t, ctx, r, "worktree", "list"); strings.Contains(list, wt) {
		t.Fatalf("worktree still registered:\n%s", list)
	}
	if err := AddDetachedWorktree(ctx, r, filepath.Join(t.TempDir(), "bad"), "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("unknown sha accepted")
	}
}

// commitFile writes name=body in dir and commits it, returning the new HEAD.
func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := CommitAll(context.Background(), dir, msg)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// bareRemote creates a bare repository, adds it to r as "origin" and pushes main to it.
func bareRemote(t *testing.T, r string) string {
	t.Helper()
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, ctx, r, "init", "-q", "--bare", "-b", "main", bare)
	mustRun(t, ctx, r, "remote", "add", "origin", bare)
	mustRun(t, ctx, r, "push", "-q", "origin", "main")
	return bare
}

func TestFetchGetsRemoteBranches(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	bare := bareRemote(t, r)
	// Another clone moves the remote's main and adds a branch.
	other := filepath.Join(t.TempDir(), "other")
	mustRun(t, ctx, r, "clone", "-q", bare, other)
	tip := commitFile(t, other, "x.txt", "x\n", "x")
	mustRun(t, ctx, other, "push", "-q", "origin", "main", "main:side")

	if err := Fetch(ctx, r, "origin"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"origin/main", "origin/side"} {
		if got, err := RevParse(ctx, r, ref); err != nil || got != tip {
			t.Errorf("%s = %q, %v; want %s", ref, got, err, tip)
		}
	}
	if err := Fetch(ctx, r, "nosuch"); err == nil {
		t.Error("Fetch of an unknown remote succeeded")
	}
}

func TestMergeConflictsCleanAndConflicting(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	mustRun(t, ctx, r, "branch", "clean")
	mustRun(t, ctx, r, "branch", "clash")
	mainTip := commitFile(t, r, "README", "main\n", "main edit")

	mustRun(t, ctx, r, "checkout", "-q", "clean")
	commitFile(t, r, "other.txt", "o\n", "other")
	mustRun(t, ctx, r, "checkout", "-q", "clash")
	commitFile(t, r, "README", "clash\n", "clash edit")
	commitFile(t, r, "b.txt", "b\n", "b")
	mustRun(t, ctx, r, "checkout", "-q", "main")

	if files, err := MergeConflicts(ctx, r, "main", "clean"); err != nil || files != nil {
		t.Fatalf("clean = %v, %v", files, err)
	}
	files, err := MergeConflicts(ctx, r, "main", "clash")
	if err != nil || len(files) != 1 || files[0] != "README" {
		t.Fatalf("clash = %v, %v", files, err)
	}
	// Nothing moved and the work tree is untouched.
	if got, _ := RevParse(ctx, r, "HEAD"); got != mainTip {
		t.Fatal("HEAD moved")
	}
	if changed, _ := HasChanges(ctx, r); changed {
		t.Fatal("work tree changed")
	}
	if _, err := MergeConflicts(ctx, r, "main", "nosuch"); err == nil {
		t.Fatal("unknown branch reported as clean")
	}
}

func TestCommitTreeAndTreeOf(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no global identity
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	r := repo(t)
	parent, _ := RevParse(ctx, r, "main")
	mustRun(t, ctx, r, "checkout", "-q", "-b", "side")
	side := commitFile(t, r, "s.txt", "s\n", "side")
	mustRun(t, ctx, r, "checkout", "-q", "main")

	tree, err := TreeOf(ctx, r, side)
	if err != nil || len(tree) != 40 {
		t.Fatalf("TreeOf = %q, %v", tree, err)
	}
	if want := mustRun(t, ctx, r, "rev-parse", side+"^{tree}"); tree != want {
		t.Fatalf("TreeOf = %s, want %s", tree, want)
	}
	// No identity in the repository: mill's own is used; hooks never run.
	if err := os.WriteFile(filepath.Join(r, ".git", "hooks", "commit-msg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sha, err := CommitTree(ctx, r, tree, parent, "squash of side\n\nMill-Task: x\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, ctx, r, "log", "-1", "--format=%T %P %an <%ae> %s", sha); got != tree+" "+parent+" mill <mill@localhost> squash of side" {
		t.Fatalf("commit = %q", got)
	}
	if got, _ := RevParse(ctx, r, "main"); got != parent {
		t.Fatal("CommitTree moved main")
	}
	root, err := CommitTree(ctx, r, tree, "", "root")
	if err != nil || mustRun(t, ctx, r, "log", "-1", "--format=%P", root) != "" {
		t.Fatalf("root commit %s, %v", root, err)
	}
	if _, err := TreeOf(ctx, r, "nosuch"); err == nil {
		t.Fatal("TreeOf of an unknown rev succeeded")
	}
}

func TestPushReviewUsesLease(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	bare := bareRemote(t, r)
	first, _ := RevParse(ctx, r, "main")
	second := commitFile(t, r, "a.txt", "a\n", "a")
	third := commitFile(t, r, "b.txt", "b\n", "b")

	// expected "" requires the branch not to exist.
	if err := PushReview(ctx, r, "origin", first, "review/g1/a", ""); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, ctx, bare, "rev-parse", "refs/heads/review/g1/a"); got != first {
		t.Fatalf("remote review/g1/a = %s, want %s", got, first)
	}
	if err := PushReview(ctx, r, "origin", second, "review/g1/a", ""); err == nil {
		t.Fatal("push with empty lease over an existing branch succeeded")
	}
	// The right lease moves it.
	if err := PushReview(ctx, r, "origin", second, "review/g1/a", first); err != nil {
		t.Fatal(err)
	}
	// A stale expected sha is rejected by git and the branch stays.
	if err := PushReview(ctx, r, "origin", third, "review/g1/a", first); err == nil {
		t.Fatal("push with a stale lease succeeded")
	}
	if got := mustRun(t, ctx, bare, "rev-parse", "refs/heads/review/g1/a"); got != second {
		t.Fatalf("remote review/g1/a = %s, want %s", got, second)
	}
	if err := PushReview(ctx, r, "origin", first, "review/g1/a", second); err != nil {
		t.Fatalf("forced push with the right lease: %v", err)
	}
}

func TestPushReviewRefusesNonReviewBranchesWithoutGit(t *testing.T) {
	ctx := context.Background()
	// Not a repository: any git call would fail with git's own error.
	dir := filepath.Join(t.TempDir(), "missing")
	for _, b := range []string{"main", "refs/heads/review/x", "reviews/x", "mill/g1/a", "review/", "Review/x", ""} {
		err := PushReview(ctx, dir, "origin", "0123456789012345678901234567890123456789", b, "")
		if err == nil || !strings.Contains(err.Error(), "only pushes review/") || strings.Contains(err.Error(), "git ") {
			t.Errorf("PushReview(%q) = %v", b, err)
		}
	}
}

func TestDeleteReviewRefusesNonReviewBranchesWithoutGit(t *testing.T) {
	ctx := context.Background()
	// Not a repository: any git call would fail with git's own error.
	dir := filepath.Join(t.TempDir(), "missing")
	for _, b := range []string{"main", "refs/heads/review/x", "reviews/x", "mill/g1/a", "review/", "Review/x", ""} {
		err := DeleteReview(ctx, dir, "origin", b, "0123456789012345678901234567890123456789")
		if err == nil || !strings.Contains(err.Error(), "only deletes review/") || strings.Contains(err.Error(), "git ") {
			t.Errorf("DeleteReview(%q) = %v", b, err)
		}
	}
}

func TestPatchLanded(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	mustRun(t, ctx, r, "checkout", "-q", "-b", "task")
	c := commitFile(t, r, "f.txt", "feature\n", "feature")
	other := commitFile(t, r, "g.txt", "other\n", "other")
	mustRun(t, ctx, r, "checkout", "-q", "main")
	commitFile(t, r, "unrelated.txt", "u\n", "unrelated")

	if landed, err := PatchLanded(ctx, r, "main", c); err != nil || landed {
		t.Fatalf("before squash = %v, %v", landed, err)
	}
	// A squash of the commit's change lands on main under a different sha.
	mustRun(t, ctx, r, "checkout", "-q", c, "--", "f.txt")
	if _, err := CommitAll(ctx, r, "squashed feature"); err != nil {
		t.Fatal(err)
	}
	if landed, err := PatchLanded(ctx, r, "main", c); err != nil || !landed {
		t.Fatalf("after squash = %v, %v", landed, err)
	}
	if landed, err := PatchLanded(ctx, r, "main", other); err != nil || landed {
		t.Fatalf("other commit = %v, %v", landed, err)
	}
	if _, err := PatchLanded(ctx, r, "main", "nosuch"); err == nil {
		t.Fatal("unknown commit succeeded")
	}
}

func TestFindTrailer(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	old := commitFile(t, r, "a.txt", "1\n", "first\n\nMill-Task: g1/a\n")
	newer := commitFile(t, r, "a.txt", "2\n", "second\n\nMill-Task: g1/a\nMill-Attempt: 2\n")
	commitFile(t, r, "b.txt", "1\n", "prefix\n\nMill-Task: g1/ab\n")
	commitFile(t, r, "c.txt", "1\n", "mentions Mill-Task: g1/a in the subject\n")

	sha, ok, err := FindTrailer(ctx, r, "main", "Mill-Task: g1/a")
	if err != nil || !ok || sha != newer {
		t.Fatalf("FindTrailer = %s, %v, %v; want %s", sha, ok, err, newer)
	}
	if sha, ok, err := FindTrailer(ctx, r, old, "Mill-Task: g1/a"); err != nil || !ok || sha != old {
		t.Fatalf("from old = %s, %v, %v", sha, ok, err)
	}
	if _, ok, err := FindTrailer(ctx, r, "main", "Mill-Task: g1/b"); err != nil || ok {
		t.Fatalf("missing trailer = %v, %v", ok, err)
	}
	if _, _, err := FindTrailer(ctx, r, "nosuch", "Mill-Task: g1/a"); err == nil {
		t.Fatal("unknown rev succeeded")
	}
}

func TestFindPatchReturnsTheSquashCommit(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	mustRun(t, ctx, r, "checkout", "-q", "-b", "task")
	c := commitFile(t, r, "f.txt", "feature\n", "feature")
	other := commitFile(t, r, "g.txt", "other\n", "other")
	mustRun(t, ctx, r, "checkout", "-q", "main")
	commitFile(t, r, "unrelated.txt", "u\n", "unrelated")
	mustRun(t, ctx, r, "checkout", "-q", c, "--", "f.txt")
	squash, err := CommitAll(ctx, r, "squashed feature")
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, r, "later.txt", "l\n", "later")

	if sha, ok, err := FindPatch(ctx, r, "main", c); err != nil || !ok || sha != squash {
		t.Fatalf("FindPatch = %s, %v, %v; want %s", sha, ok, err, squash)
	}
	if sha, ok, err := FindPatch(ctx, r, "main", other); err != nil || ok {
		t.Fatalf("FindPatch(other) = %s, %v, %v; want none", sha, ok, err)
	}
}

func TestContainingCommitFindsMergeOrFastForward(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	mustRun(t, ctx, r, "checkout", "-q", "-b", "task")
	c := commitFile(t, r, "f.txt", "feature\n", "feature")
	mustRun(t, ctx, r, "checkout", "-q", "main")
	commitFile(t, r, "unrelated.txt", "u\n", "unrelated")
	if err := Merge(ctx, r, "task", "merge task"); err != nil {
		t.Fatal(err)
	}
	merge := mustRun(t, ctx, r, "rev-parse", "HEAD")
	commitFile(t, r, "later.txt", "l\n", "later")
	if got, err := ContainingCommit(ctx, r, "main", c); err != nil || got != merge {
		t.Fatalf("ContainingCommit after merge = %s, %v; want %s", got, err, merge)
	}

	mustRun(t, ctx, r, "checkout", "-q", "-b", "ff")
	d := commitFile(t, r, "d.txt", "d\n", "ff commit")
	mustRun(t, ctx, r, "checkout", "-q", "main")
	mustRun(t, ctx, r, "merge", "-q", "--ff-only", "ff")
	commitFile(t, r, "after.txt", "a\n", "after")
	if got, err := ContainingCommit(ctx, r, "main", d); err != nil || got != d {
		t.Fatalf("ContainingCommit after fast-forward = %s, %v; want %s", got, err, d)
	}
}

// divergedRepo returns a repo with branch feat (two commits after base: one changing
// shared.txt, one adding feat.txt) checked out, main changing shared.txt differently when
// conflict is set, and the shas of base, main and feat.
func divergedRepo(t *testing.T, conflict bool) (dir, base, main, feat string) {
	t.Helper()
	ctx := context.Background()
	dir = repo(t)
	base = commitFile(t, dir, "shared.txt", "a\nb\nbase\nc\nd\n", "add shared")
	mustRun(t, ctx, dir, "checkout", "-q", "-b", "feat")
	commitFile(t, dir, "shared.txt", "a\nb\nfeat\nc\nd\n", "feat shared\n\nMill-Task: g1/feat\n")
	feat = commitFile(t, dir, "feat.txt", "feat\n", "add feat.txt\n\nMill-Task: g1/feat\nMill-Attempt: 2\n")
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	if conflict {
		main = commitFile(t, dir, "shared.txt", "a\nb\nmain\nc\nd\n", "main shared")
	} else {
		main = commitFile(t, dir, "main.txt", "main\n", "main file")
	}
	mustRun(t, ctx, dir, "checkout", "-q", "feat")
	return dir, base, main, feat
}

func TestRebaseStartWithoutConflictFinishes(t *testing.T) {
	ctx := context.Background()
	dir, base, main, _ := divergedRepo(t, false)
	st, err := RebaseStart(ctx, dir, main, base, false)
	if err != nil {
		t.Fatal(err)
	}
	head := mustRun(t, ctx, dir, "rev-parse", "HEAD")
	if !st.Done || st.Head != head || len(st.Files) != 0 {
		t.Fatalf("state = %+v, HEAD = %s", st, head)
	}
	if parent := mustRun(t, ctx, dir, "rev-parse", "HEAD~2"); parent != main {
		t.Fatalf("HEAD~2 = %s, want onto %s", parent, main)
	}
	if subjects := mustRun(t, ctx, dir, "log", "--format=%s", main+"..HEAD"); subjects != "add feat.txt\nfeat shared" {
		t.Fatalf("replayed commits = %q", subjects)
	}
}

func TestRebaseStartStopsOnConflictWithoutAborting(t *testing.T) {
	ctx := context.Background()
	dir, base, main, _ := divergedRepo(t, true)
	featShared := mustRun(t, ctx, dir, "rev-parse", "feat~1")
	st, err := RebaseStart(ctx, dir, main, base, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Done || st.Stopped != featShared || len(st.Files) != 1 || st.Files[0] != "shared.txt" {
		t.Fatalf("state = %+v, want stop at %s on shared.txt", st, featShared)
	}
	if rh := mustRun(t, ctx, dir, "rev-parse", "REBASE_HEAD"); rh != featShared {
		t.Fatalf("REBASE_HEAD = %s: rebase not in progress", rh)
	}
	if in, err := rebaseInProgress(ctx, dir); err != nil || !in {
		t.Fatalf("rebase in progress = %v, %v", in, err)
	}
}

func TestRebaseContinueFinishesWithoutEditor(t *testing.T) {
	ctx := context.Background()
	dir, base, main, _ := divergedRepo(t, true)
	t.Setenv("EDITOR", "false") // an editor would fail the continue
	t.Setenv("VISUAL", "false")
	if _, err := RebaseStart(ctx, dir, main, base, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("a\nb\nmain feat\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := RebaseContinue(ctx, dir, []string{"shared.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || st.Head != mustRun(t, ctx, dir, "rev-parse", "HEAD") {
		t.Fatalf("state = %+v", st)
	}
	if branch := mustRun(t, ctx, dir, "symbolic-ref", "--short", "HEAD"); branch != "feat" {
		t.Fatalf("on %q after rebase, want feat", branch)
	}
	if got := mustRun(t, ctx, dir, "show", "HEAD~1:shared.txt"); got != "a\nb\nmain feat\nc\nd" {
		t.Fatalf("resolved shared.txt = %q", got)
	}
	if parent := mustRun(t, ctx, dir, "rev-parse", "HEAD~2"); parent != main {
		t.Fatalf("HEAD~2 = %s, want %s", parent, main)
	}
}

func TestRebaseAbortRestoresOriginalHead(t *testing.T) {
	ctx := context.Background()
	dir, base, main, feat := divergedRepo(t, true)
	if _, err := RebaseStart(ctx, dir, main, base, false); err != nil {
		t.Fatal(err)
	}
	if err := RebaseAbort(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if head := mustRun(t, ctx, dir, "rev-parse", "HEAD"); head != feat {
		t.Fatalf("HEAD = %s, want %s", head, feat)
	}
	if in, err := rebaseInProgress(ctx, dir); err != nil || in {
		t.Fatalf("rebase in progress = %v, %v", in, err)
	}
	if err := RebaseAbort(ctx, dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("abort without a rebase: err = %v, want it to name %s", err, dir)
	}
}

func TestRebaseStartReusesRecordedResolutionWithRerere(t *testing.T) {
	ctx := context.Background()
	dir, base, main, feat := divergedRepo(t, true)
	st, err := RebaseStart(ctx, dir, main, base, true)
	if err != nil || st.Done {
		t.Fatalf("first rebase = %+v, %v; want a stop", st, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("a\nb\nmain feat\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err = RebaseContinue(ctx, dir, st.Files, true); err != nil || !st.Done {
		t.Fatalf("continue = %+v, %v", st, err)
	}
	first := st.Head

	// Same conflict again: rerere resolves it and the rebase finishes by itself.
	mustRun(t, ctx, dir, "reset", "-q", "--hard", feat)
	st, err = RebaseStart(ctx, dir, main, base, true)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || st.RerereResolved < 1 || len(st.Files) != 0 {
		t.Fatalf("state = %+v, want Done with RerereResolved >= 1", st)
	}
	if got, want := mustRun(t, ctx, dir, "rev-parse", "HEAD^{tree}"), mustRun(t, ctx, dir, "rev-parse", first+"^{tree}"); got != want {
		t.Fatalf("tree = %s, want the recorded resolution's %s", got, want)
	}
}

func TestMergeStartReportsConflictAndContinueCommits(t *testing.T) {
	ctx := context.Background()
	dir, _, _, feat := divergedRepo(t, true)
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	st, err := MergeStart(ctx, dir, "feat", "merge feat", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Done || len(st.Files) != 1 || st.Files[0] != "shared.txt" {
		t.Fatalf("state = %+v", st)
	}
	if _, err := Run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		t.Fatal("merge not left in progress")
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := MergeContinue(ctx, dir, st.Files, "merge feat")
	if err != nil {
		t.Fatal(err)
	}
	if head := mustRun(t, ctx, dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("HEAD = %s, want %s", head, sha)
	}
	if parents := strings.Fields(mustRun(t, ctx, dir, "log", "-1", "--format=%P")); len(parents) != 2 || parents[1] != feat {
		t.Fatalf("parents = %v, want a merge of %s", parents, feat)
	}
	if msg := mustRun(t, ctx, dir, "log", "-1", "--format=%s"); msg != "merge feat" {
		t.Fatalf("message = %q", msg)
	}
	if has, _ := HasChanges(ctx, dir); has {
		t.Fatal("work tree dirty after the merge")
	}
	if _, err := MergeContinue(ctx, dir, nil, "again"); err == nil {
		t.Fatal("MergeContinue without a merge in progress succeeded")
	}
}

func TestMergeStartWithoutConflictCommits(t *testing.T) {
	ctx := context.Background()
	dir, _, _, _ := divergedRepo(t, false)
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	st, err := MergeStart(ctx, dir, "feat", "merge feat", false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || st.Head != mustRun(t, ctx, dir, "rev-parse", "HEAD") {
		t.Fatalf("state = %+v", st)
	}
	if parents := mustRun(t, ctx, dir, "log", "-1", "--format=%P"); len(strings.Fields(parents)) != 2 {
		t.Fatalf("not a merge commit, parents = %q", parents)
	}
}

func TestMergeAbortRestoresOriginalHead(t *testing.T) {
	ctx := context.Background()
	dir, _, main, _ := divergedRepo(t, true)
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	if _, err := MergeStart(ctx, dir, "feat", "merge feat", false); err != nil {
		t.Fatal(err)
	}
	if err := MergeAbort(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if head := mustRun(t, ctx, dir, "rev-parse", "HEAD"); head != main {
		t.Fatalf("HEAD = %s, want %s", head, main)
	}
	if has, _ := HasChanges(ctx, dir); has {
		t.Fatal("work tree dirty after abort")
	}
}

func TestMergeStartReusesRecordedResolutionWithRerere(t *testing.T) {
	ctx := context.Background()
	dir, _, main, _ := divergedRepo(t, true)
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	st, err := MergeStart(ctx, dir, "feat", "merge feat", true)
	if err != nil || st.Done {
		t.Fatalf("first merge = %+v, %v; want conflicts", st, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeContinue(ctx, dir, st.Files, "merge feat"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, dir, "reset", "-q", "--hard", main)
	st, err = MergeStart(ctx, dir, "feat", "merge feat", true)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || !st.RerereResolved {
		t.Fatalf("state = %+v, want Done by rerere", st)
	}
	if got := mustRun(t, ctx, dir, "show", "HEAD:shared.txt"); got != "merged" {
		t.Fatalf("shared.txt = %q", got)
	}
}

func TestConflictHunksShowsOnlyMarkerRegionsWithContext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	lines = append(lines[:10], append([]string{"<<<<<<< HEAD", "ours", "=======", "theirs", ">>>>>>> feat"}, lines[10:]...)...)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ConflictHunks(ctx, dir, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := "@@ f.txt lines 8-18 @@\nline8\nline9\nline10\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> feat\nline11\nline12\nline13\n"
	if got != want {
		t.Fatalf("hunks =\n%s\nwant\n%s", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.txt"), []byte("no conflict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ConflictHunks(ctx, dir, "clean.txt"); err != nil || got != "" {
		t.Fatalf("clean file hunks = %q, %v", got, err)
	}
	if _, err := ConflictHunks(ctx, dir, "missing.txt"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestHasConflictMarkersListsFilesStillMarked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		"open.txt":   "x\n<<<<<<< HEAD\na\n",
		"middle.txt": "x\n=======\ny\n",
		"close.txt":  "x\n>>>>>>> feat\n",
		"clean.txt":  "x\n== not a marker ==\n<<<<<<<< eight\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := HasConflictMarkers(ctx, dir, []string{"open.txt", "clean.txt", "middle.txt", "gone.txt", "close.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "open.txt,middle.txt,close.txt" {
		t.Fatalf("marked = %v", got)
	}
}

func TestCommitSummariesListsSubjectsAndTrailersOldestFirst(t *testing.T) {
	ctx := context.Background()
	dir, base, _, feat := divergedRepo(t, false)
	got, err := CommitSummaries(ctx, dir, base, feat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("summaries = %+v", got)
	}
	if got[0].Subject != "feat shared" || strings.Join(got[0].Trailers, "|") != "Mill-Task: g1/feat" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].SHA != feat || got[1].Subject != "add feat.txt" || strings.Join(got[1].Trailers, "|") != "Mill-Task: g1/feat|Mill-Attempt: 2" {
		t.Fatalf("second = %+v", got[1])
	}
	if none, err := CommitSummaries(ctx, dir, feat, feat); err != nil || len(none) != 0 {
		t.Fatalf("empty range = %+v, %v", none, err)
	}
}

func TestReplayOntoAppliesNetChangeOfAttemptsThatRevertEachOther(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	base := mustRun(t, ctx, dir, "rev-parse", "HEAD")
	mustRun(t, ctx, dir, "checkout", "-q", "-b", "feat")
	commitFile(t, dir, "feat.txt", "feat\n", "mill: x attempt 1\n\nMill-Task: x\nMill-Attempt: 1")
	commitFile(t, dir, "clash.txt", "feat\n", "mill: x attempt 2\n\nMill-Task: x\nMill-Attempt: 2")
	if err := os.Remove(filepath.Join(dir, "clash.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitAll(ctx, dir, "mill: x attempt 3\n\nMill-Task: x\nMill-Attempt: 3"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, "feat.txt", "feat 5\n", "mill: x attempt 5\n\nMill-Task: x\nMill-Attempt: 5")
	mustRun(t, ctx, dir, "checkout", "-q", "main")
	main := commitFile(t, dir, "clash.txt", "main\n", "main clash")
	mustRun(t, ctx, dir, "checkout", "-q", "feat")

	head, err := ReplayOnto(ctx, dir, "x", main, base)
	if err != nil {
		t.Fatal(err)
	}
	if parent := mustRun(t, ctx, dir, "rev-parse", head+"^"); parent != main {
		t.Fatalf("parent = %s, want %s", parent, main)
	}
	if files := mustRun(t, ctx, dir, "diff", "--name-status", main, head); files != "A\tfeat.txt" {
		t.Fatalf("net change = %q", files)
	}
	if got := mustRun(t, ctx, dir, "show", head+":feat.txt"); got != "feat 5" {
		t.Fatalf("feat.txt = %q", got)
	}
	if subject := mustRun(t, ctx, dir, "log", "-1", "--format=%s", head); subject != "mill: x attempts 1-3, 5 (squashed for restack)" {
		t.Fatalf("subject = %q", subject)
	}
	// The squashed commit keeps the trailers, so a later restack names the same attempts.
	if got := mustRun(t, ctx, dir, "log", "-1", "--format=%(trailers:key=Mill-Attempt,valueonly,separator=%x2C)", head); got != "1,2,3,5" {
		t.Fatalf("attempt trailers = %q", got)
	}
}

func TestReplayOntoConflictLeavesBranchAtOldHead(t *testing.T) {
	ctx := context.Background()
	dir, base, main, feat := divergedRepo(t, true)
	_, err := ReplayOnto(ctx, dir, "x", main, base)
	var ce *RebaseConflictError
	if !errors.As(err, &ce) || len(ce.Files) != 1 || ce.Files[0] != "shared.txt" {
		t.Fatalf("err = %v, want a conflict in shared.txt", err)
	}
	if head := mustRun(t, ctx, dir, "rev-parse", "HEAD"); head != feat {
		t.Fatalf("HEAD = %s, want the old head %s", head, feat)
	}
	if branch := mustRun(t, ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "feat" {
		t.Fatalf("checked out %q, want feat", branch)
	}
	if st := mustRun(t, ctx, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean: %q", st)
	}
}

func TestSquashOwnWithoutAttemptTrailersNamesOwnCommits(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	base := mustRun(t, ctx, dir, "rev-parse", "HEAD")
	commitFile(t, dir, "a.txt", "a\n", "plain")
	old, sq, err := SquashOwn(ctx, dir, "x", base)
	if err != nil {
		t.Fatal(err)
	}
	if old == sq || mustRun(t, ctx, dir, "rev-parse", "HEAD") != sq || mustRun(t, ctx, dir, "rev-parse", sq+"^") != base {
		t.Fatalf("old %s, squashed %s", old, sq)
	}
	if subject := mustRun(t, ctx, dir, "log", "-1", "--format=%s", sq); subject != "mill: x own commits (squashed for restack)" {
		t.Fatalf("subject = %q", subject)
	}
	if err := Unsquash(ctx, dir, old, sq); err != nil {
		t.Fatal(err)
	}
	if head := mustRun(t, ctx, dir, "rev-parse", "HEAD"); head != old {
		t.Fatalf("HEAD = %s after Unsquash, want %s", head, old)
	}
}

func TestApplyTreeCarriesOnlyTheChangeOntoTheTarget(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	base := mustRun(t, ctx, r, "rev-parse", "HEAD")
	mustRun(t, ctx, r, "branch", "feat")
	mustRun(t, ctx, r, "branch", "clash")
	mainTip := commitFile(t, r, "other.txt", "main\n", "main gains other.txt")
	mustRun(t, ctx, r, "checkout", "-q", "feat")
	featTip := commitFile(t, r, "feat.txt", "f\n", "feat")
	mustRun(t, ctx, r, "checkout", "-q", "clash")
	clashTip := commitFile(t, r, "other.txt", "clash\n", "clash")
	mustRun(t, ctx, r, "checkout", "-q", "main")

	tree, conflicts, err := ApplyTree(ctx, r, base, featTip, mainTip)
	if err != nil || conflicts != nil {
		t.Fatalf("apply = %v, %v", conflicts, err)
	}
	if got, err := ChangedPaths(ctx, r, mainTip, tree); err != nil || len(got) != 1 || got[0] != "feat.txt" {
		t.Errorf("applied tree changes %v (%v), want only feat.txt", got, err)
	}
	if got, err := ChangedPaths(ctx, r, featTip, tree); err != nil || len(got) != 1 || got[0] != "other.txt" {
		t.Errorf("applied tree differs from the head in %v (%v), want only other.txt kept from main", got, err)
	}
	if tree, conflicts, err := ApplyTree(ctx, r, base, clashTip, mainTip); err != nil || tree != "" || len(conflicts) != 1 || conflicts[0] != "other.txt" {
		t.Errorf("conflicting apply = %q, %v, %v; want other.txt", tree, conflicts, err)
	}
	if got, _ := RevParse(ctx, r, "HEAD"); got != mainTip {
		t.Error("HEAD moved")
	}
}
