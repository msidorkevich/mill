// Package gitx runs the plain git commands mill needs (GT1). No forge APIs.
package gitx

import (
	"bytes"
	"context"
	"errors"
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

// IsAncestor reports whether commit ancestor is reachable from descendant.
func IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w: %s", ancestor, descendant, err, strings.TrimSpace(stderr.String()))
}

// RebaseConflictError reports a rebase that stopped on conflicts. The rebase has been
// aborted and the branch is as it was.
type RebaseConflictError struct {
	Onto  string
	Files []string
}

func (e *RebaseConflictError) Error() string {
	return fmt.Sprintf("rebasing onto %s conflicts in %s", e.Onto, strings.Join(e.Files, ", "))
}

// RebaseOnto replays the commits after `upstream` on the branch checked out in dir onto
// `onto` (git rebase --onto), skipping hooks and signing. When the repository has no identity
// configured, mill's own is used. On conflicts the rebase is aborted and a
// *RebaseConflictError lists the files. It returns the new HEAD sha.
func RebaseOnto(ctx context.Context, dir, onto, upstream string) (string, error) {
	args := []string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	if email, _ := Run(ctx, dir, "config", "user.email"); email == "" {
		args = append(args, "-c", "user.name=mill", "-c", "user.email=mill@localhost")
	}
	args = append(args, "rebase", "--no-verify", "--onto", onto, upstream)
	_, rebaseErr := Run(ctx, dir, args...)
	if rebaseErr == nil {
		return RevParse(ctx, dir, "HEAD")
	}
	files, lsErr := Run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	_, inProgress := Run(ctx, dir, "rev-parse", "-q", "--verify", "REBASE_HEAD")
	if inProgress == nil {
		if _, err := Run(ctx, dir, "rebase", "--abort"); err != nil {
			return "", fmt.Errorf("%w; and aborting the rebase failed, run `git rebase --abort` in %s: %v", rebaseErr, dir, err)
		}
	}
	if lsErr != nil || files == "" {
		return "", rebaseErr
	}
	return "", &RebaseConflictError{Onto: onto, Files: strings.Split(files, "\n")}
}

// runExit runs git in dir and returns trimmed stdout and git's exit code. err is set only
// when git could not run or was killed (not for a non-zero exit).
func runExit(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() < 0) {
		return "", -1, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	code := 0
	if ee != nil {
		code = ee.ExitCode()
	}
	if code != 0 && code != 1 {
		return "", code, fmt.Errorf("git %s: exit status %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), code, nil
}

// identity returns the -c options for a commit without hooks or signing, adding mill's own
// identity when the repository has none.
func identity(ctx context.Context, dir string) []string {
	args := []string{"-c", "commit.gpgsign=false"}
	if email, _ := Run(ctx, dir, "config", "user.email"); email == "" {
		args = append(args, "-c", "user.name=mill", "-c", "user.email=mill@localhost")
	}
	return args
}

// Fetch fetches the branches of remote (its configured remote-tracking refs).
func Fetch(ctx context.Context, dir, remote string) error {
	if strings.HasPrefix(remote, "-") || remote == "" {
		return fmt.Errorf("fetch: invalid remote %q", remote)
	}
	_, err := Run(ctx, dir, "fetch", "--quiet", "--prune", remote)
	return err
}

// MergeConflicts reports the paths that conflict when merging branch into base, without
// touching any work tree or ref. It returns nil when they merge cleanly.
func MergeConflicts(ctx context.Context, dir, base, branch string) ([]string, error) {
	out, code, err := runExit(ctx, dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", base, branch)
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return nil, nil
	}
	// -z output: the tree id, then each conflicted path, NUL-terminated.
	parts := strings.Split(out, "\x00")
	var files []string
	for _, f := range parts[1:] {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("git merge-tree %s %s reported a conflict but listed no paths", base, branch)
	}
	return files, nil
}

// TreeOf returns the tree id of rev.
func TreeOf(ctx context.Context, dir, rev string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{tree}")
}

// CommitTree creates a commit of tree with parent (none when parent is empty) and message,
// without hooks or signing, and returns its sha. No ref moves. When the repository has no
// identity configured, mill's own is used.
func CommitTree(ctx context.Context, dir, tree, parent, message string) (string, error) {
	args := append(identity(ctx, dir), "commit-tree", tree)
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-F", "-")
	return RunInput(ctx, dir, message, args...)
}

// ReviewPrefix is the only branch namespace mill pushes to (SE2).
const ReviewPrefix = "review/"

// PushReview pushes sha to refs/heads/<branch> on remote, only if the remote branch is still
// at expected ("" means it must not exist yet). It refuses, without running git, any branch
// outside review/: mill never pushes anywhere else. This is mill's only push.
func PushReview(ctx context.Context, dir, remote, sha, branch, expected string) error {
	if !strings.HasPrefix(branch, ReviewPrefix) || len(branch) == len(ReviewPrefix) {
		return fmt.Errorf("refusing to push to branch %q: mill only pushes review/<name> branches", branch)
	}
	if remote == "" || strings.HasPrefix(remote, "-") || sha == "" || strings.HasPrefix(sha, "-") {
		return fmt.Errorf("refusing to push %q to remote %q: invalid remote or sha", sha, remote)
	}
	ref := "refs/heads/" + branch
	_, err := Run(ctx, dir, "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+expected, remote, sha+":"+ref)
	return err
}

// PatchLanded reports whether upstream contains a commit with the same patch as the single
// commit `commit` (git cherry marks it with "-"), or contains the commit itself.
func PatchLanded(ctx context.Context, dir, upstream, commit string) (bool, error) {
	out, err := Run(ctx, dir, "cherry", upstream, commit, commit+"^")
	if err != nil {
		return false, err
	}
	if out == "" {
		// git cherry lists nothing when the commit itself is already in upstream.
		in, err := IsAncestor(ctx, dir, commit, upstream)
		if err != nil {
			return false, err
		}
		if !in {
			return false, fmt.Errorf("git cherry %s %s %s^ listed no commit; cannot tell whether it landed", upstream, commit, commit)
		}
		return true, nil
	}
	return strings.HasPrefix(out, "-"), nil
}

// FindTrailer returns the newest commit reachable from rev whose message has a line exactly
// equal to trailer (e.g. "Mill-Task: g1/a"). ok is false when there is none.
func FindTrailer(ctx context.Context, dir, rev, trailer string) (sha string, ok bool, err error) {
	out, err := Run(ctx, dir, "log", "-z", "--format=%H%n%B", "--fixed-strings", "--grep="+trailer, rev, "--")
	if err != nil {
		return "", false, err
	}
	for _, rec := range strings.Split(out, "\x00") {
		hash, body, _ := strings.Cut(strings.TrimLeft(rec, "\n"), "\n")
		if hash == "" {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.TrimSpace(line) == trailer {
				return hash, true, nil
			}
		}
	}
	return "", false, nil
}

// ContainingCommit returns the commit on rev's first-parent history that brought commit into
// rev: commit itself after a fast-forward, or the merge commit that merged it. commit must be
// an ancestor of rev.
func ContainingCommit(ctx context.Context, dir, rev, commit string) (string, error) {
	args := []string{"rev-list", "--first-parent", rev}
	if _, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", commit+"^"); err == nil {
		args = append(args, "^"+commit+"^")
	}
	out, err := Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	first := map[string]bool{}
	for _, c := range strings.Fields(out) {
		first[c] = true
	}
	full, err := RevParse(ctx, dir, commit)
	if err != nil {
		return "", err
	}
	if first[full] {
		return full, nil
	}
	out, err = Run(ctx, dir, "rev-list", "--reverse", "--ancestry-path", full+".."+rev)
	if err != nil {
		return "", err
	}
	for _, c := range strings.Fields(out) {
		if first[c] {
			return c, nil
		}
	}
	return "", fmt.Errorf("no commit on the first-parent history of %s contains %s", rev, commit)
}

// FindPatch returns the commit reachable from upstream (and not from commit's parent) whose
// patch is the same as the single commit `commit` (same `git patch-id --stable`). ok is false
// when there is none.
func FindPatch(ctx context.Context, dir, upstream, commit string) (sha string, ok bool, err error) {
	want, err := patchIDs(ctx, dir, "show", "--no-color", "--no-ext-diff", "--format=commit %H", commit)
	if err != nil {
		return "", false, err
	}
	if len(want) != 1 {
		return "", false, fmt.Errorf("commit %s has no patch to compare", commit)
	}
	var id string
	for k := range want {
		id = k
	}
	got, err := patchIDs(ctx, dir, "log", "-p", "--no-merges", "--no-color", "--no-ext-diff", "--format=commit %H",
		upstream, "^"+commit+"^", "--")
	if err != nil {
		return "", false, err
	}
	sha, ok = got[id]
	return sha, ok, nil
}

// patchIDs runs git with args (a log-like patch output) through `git patch-id --stable` and
// maps each patch id to its commit. When several commits share a patch, the first listed wins.
func patchIDs(ctx context.Context, dir string, args ...string) (map[string]string, error) {
	patches, err := Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if patches == "" {
		return out, nil
	}
	ids, err := RunInput(ctx, dir, patches+"\n", "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(ids, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if _, seen := out[f[0]]; !seen {
			out[f[0]] = f[1]
		}
	}
	return out, nil
}
