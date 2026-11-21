package gitx

import (
	"context"
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
