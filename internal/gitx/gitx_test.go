package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
