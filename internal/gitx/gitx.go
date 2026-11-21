// Package gitx runs the plain git commands mill needs (GT1). No forge APIs.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run executes git in dir and returns trimmed stdout. Errors include git's stderr.
//
// The environment is mill's own minus every GIT_* variable, so an inherited GIT_DIR or
// GIT_INDEX_FILE can never redirect mill's git calls to another repository.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return RunInput(ctx, dir, "", args...)
}

// RunInput is Run with stdin.
func RunInput(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// RevParse resolves rev to a full commit sha.
func RevParse(ctx context.Context, dir, rev string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// BranchExists reports whether a local branch exists.
func BranchExists(ctx context.Context, dir, name string) bool {
	_, err := Run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// AddWorktree creates a new branch at startSHA and checks it out in a new worktree at path.
func AddWorktree(ctx context.Context, repo, path, branch, startSHA string) error {
	_, err := Run(ctx, repo, "worktree", "add", "-q", "-b", branch, path, startSHA)
	return err
}

// AddWorktreeForBranch checks out an existing branch in a new worktree at path.
func AddWorktreeForBranch(ctx context.Context, repo, path, branch string) error {
	_, err := Run(ctx, repo, "worktree", "add", "-q", path, branch)
	return err
}

// AddDetachedWorktree checks out sha with a detached HEAD in a new worktree at path (no
// branch is created). Remove it with RemoveWorktree.
func AddDetachedWorktree(ctx context.Context, repo, path, sha string) error {
	_, err := Run(ctx, repo, "worktree", "add", "-q", "--detach", path, sha)
	return err
}

// IsWorktreeOn reports whether path is a work tree with branch checked out.
func IsWorktreeOn(ctx context.Context, path, branch string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	cur, err := Run(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	return err == nil && cur == branch
}

// PruneWorktrees removes git's records of worktrees whose directories are gone.
func PruneWorktrees(ctx context.Context, repo string) error {
	_, err := Run(ctx, repo, "worktree", "prune")
	return err
}

// HasChanges reports whether the work tree has staged, unstaged or untracked changes
// (ignored files excluded).
func HasChanges(ctx context.Context, dir string) (bool, error) {
	out, err := Run(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	return out != "", err
}

// CommitAll stages everything (respecting .gitignore) and commits it with msg, returning the
// new HEAD sha. Repository hooks and signing are skipped so the commit is deterministic.
// When the repository has no identity configured, mill's own is used.
func CommitAll(ctx context.Context, dir, msg string) (string, error) {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	args := []string{"-c", "commit.gpgsign=false"}
	if email, _ := Run(ctx, dir, "config", "user.email"); email == "" {
		args = append(args, "-c", "user.name=mill", "-c", "user.email=mill@localhost")
	}
	args = append(args, "commit", "-q", "--no-verify", "-F", "-")
	if _, err := RunInput(ctx, dir, msg, args...); err != nil {
		return "", err
	}
	return RevParse(ctx, dir, "HEAD")
}

// Diff returns the patch from base to HEAD.
func Diff(ctx context.Context, dir, base string) (string, error) {
	return Run(ctx, dir, "diff", "--no-color", "--no-ext-diff", base+"..HEAD")
}

// ChangedFiles lists the paths changed from `from` to HEAD, including deletions and the new
// path of renames.
func ChangedFiles(ctx context.Context, dir, from string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", "--no-color", "--no-ext-diff", "-M", "-z", from+"..HEAD")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// DiffStat returns `git diff --stat` from base to HEAD.
func DiffStat(ctx context.Context, dir, base string) (string, error) {
	return Run(ctx, dir, "diff", "--no-color", "--stat", base+"..HEAD")
}

// ConflictError reports a merge that stopped on conflicts. The merge has been aborted.
type ConflictError struct {
	Branch string
	Files  []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merging %s conflicts in %s", e.Branch, strings.Join(e.Files, ", "))
}

// Merge merges branch into the branch checked out in dir with a merge commit (no
// fast-forward), skipping hooks and signing. When the repository has no identity configured,
// mill's own is used. On conflicts the merge is aborted and a *ConflictError lists the files.
func Merge(ctx context.Context, dir, branch, message string) error {
	args := []string{"-c", "commit.gpgsign=false"}
	if email, _ := Run(ctx, dir, "config", "user.email"); email == "" {
		args = append(args, "-c", "user.name=mill", "-c", "user.email=mill@localhost")
	}
	args = append(args, "merge", "--no-ff", "--no-verify", "-q", "-m", message, branch)
	_, mergeErr := Run(ctx, dir, args...)
	if mergeErr == nil {
		return nil
	}
	files, lsErr := Run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if lsErr != nil || files == "" {
		// Not a conflict (or undecidable): clean up if a merge is in progress and report as is.
		if _, err := Run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
			if _, err := Run(ctx, dir, "merge", "--abort"); err != nil {
				return fmt.Errorf("%w; and aborting the merge failed: %v", mergeErr, err)
			}
		}
		return mergeErr
	}
	ce := &ConflictError{Branch: branch, Files: strings.Split(files, "\n")}
	if _, err := Run(ctx, dir, "merge", "--abort"); err != nil {
		return fmt.Errorf("%w; and aborting the merge failed, run `git merge --abort` in %s: %v", ce, dir, err)
	}
	return ce
}

// RemoveWorktree deletes a worktree directory and git's record of it, even when it is dirty.
func RemoveWorktree(ctx context.Context, repo, path string) error {
	_, err := Run(ctx, repo, "worktree", "remove", "--force", path)
	return err
}

// DeleteBranch force-deletes a local branch.
func DeleteBranch(ctx context.Context, repo, name string) error {
	_, err := Run(ctx, repo, "branch", "-D", "-q", name)
	return err
}
