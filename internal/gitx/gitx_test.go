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
