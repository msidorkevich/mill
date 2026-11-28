// Package gitx runs the plain git commands mill needs (GT1). No forge APIs.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	return runEnv(ctx, dir, stdin, nil, args...)
}

// runEnv is RunInput with extra environment variables (added after cleanEnv's).
func runEnv(ctx context.Context, dir, stdin string, env []string, args ...string) (string, error) {
	cmd := Command(ctx, args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started(ctx)
	if err := cmd.Run(); err != nil {
		failed(ctx, dir)
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), withCtxErr(ctx, err), strings.TrimSpace(stderr.String()))
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

// worktreeAdmin runs `git worktree <args>` in repo while holding the repository's worktree
// administration lock (lockWorktreeAdmin): git commands that change .git/worktrees race
// when run concurrently in one repository.
func worktreeAdmin(ctx context.Context, repo string, args ...string) error {
	unlock, err := lockWorktreeAdmin(ctx, repo)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = Run(ctx, repo, append([]string{"worktree"}, args...)...)
	return err
}

// AddWorktree creates a new branch at startSHA and checks it out in a new worktree at path.
func AddWorktree(ctx context.Context, repo, path, branch, startSHA string) error {
	return worktreeAdmin(ctx, repo, "add", "-q", "-b", branch, path, startSHA)
}

// AddWorktreeForBranch checks out an existing branch in a new worktree at path.
func AddWorktreeForBranch(ctx context.Context, repo, path, branch string) error {
	return worktreeAdmin(ctx, repo, "add", "-q", path, branch)
}

// AddDetachedWorktree checks out sha with a detached HEAD in a new worktree at path (no
// branch is created). Remove it with RemoveWorktree.
func AddDetachedWorktree(ctx context.Context, repo, path, sha string) error {
	return worktreeAdmin(ctx, repo, "add", "-q", "--detach", path, sha)
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
	return worktreeAdmin(ctx, repo, "prune")
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
	args := append(identity(ctx, dir), "commit", "-q", "--no-verify", "-F", "-")
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
	args := append(identity(ctx, dir), "merge", "--no-ff", "--no-verify", "-q", "-m", message, branch)
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
	return worktreeAdmin(ctx, repo, "remove", "--force", path)
}

// DeleteBranch force-deletes a local branch.
func DeleteBranch(ctx context.Context, repo, name string) error {
	_, err := Run(ctx, repo, "branch", "-D", "-q", name)
	return err
}

// IsAncestor reports whether commit ancestor is reachable from descendant.
func IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := Command(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	started(ctx)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && ctx.Err() == nil {
		return false, nil
	}
	failed(ctx, dir)
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w: %s", ancestor, descendant, withCtxErr(ctx, err), strings.TrimSpace(stderr.String()))
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
	args := append(identity(ctx, dir), "-c", "core.hooksPath=/dev/null", "rebase", "--no-verify", "--onto", onto, upstream)
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

// SquashOwn replaces the own commits of the branch checked out in dir (the commits after
// upstream) with one commit on upstream that carries the branch head's tree: their net change.
// Attempts that undo each other cancel out, so replaying the result conflicts only where the
// net change does. The squash is plumbing (commit-tree, then the branch ref moves to the new
// commit with the same tree), so the index and work tree are untouched. Its message names the
// task and the attempts it combines, read from the commits' Mill-Attempt trailers, and repeats
// those trailers so a later squash still finds them. It returns the old head, to move the
// branch back with Unsquash, and the new one; both are the head when there is nothing to squash.
func SquashOwn(ctx context.Context, dir, task, upstream string) (oldHead, newHead string, err error) {
	head, err := RevParse(ctx, dir, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read the head to squash: %w", err)
	}
	if head == upstream {
		return head, head, nil
	}
	out, err := Run(ctx, dir, "log", "--no-merges", "--format=%(trailers:key=Mill-Attempt,valueonly)", upstream+".."+head)
	if err != nil {
		return "", "", fmt.Errorf("list the own commits after %s: %w", upstream, err)
	}
	var attempts []int
	seen := map[int]bool{}
	for _, f := range strings.Fields(out) {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		attempts = append(attempts, n)
	}
	sort.Ints(attempts)
	tree, err := TreeOf(ctx, dir, head)
	if err != nil {
		return "", "", fmt.Errorf("read the tree of %s: %w", head, err)
	}
	sha, err := CommitTree(ctx, dir, tree, upstream, squashMessage(task, attempts))
	if err != nil {
		return "", "", fmt.Errorf("create the squashed commit of the own commits: %w", err)
	}
	if _, err := Run(ctx, dir, "update-ref", "-m", "mill: squash for restack", "HEAD", sha, head); err != nil {
		return "", "", fmt.Errorf("move the branch to the squashed commit %s: %w", sha, err)
	}
	return head, sha, nil
}

// Unsquash moves the branch checked out in dir from the squashed commit newHead back to
// oldHead (SquashOwn's results). Both carry the same tree, so the work tree is untouched.
func Unsquash(ctx context.Context, dir, oldHead, newHead string) error {
	if oldHead == newHead {
		return nil
	}
	if _, err := Run(ctx, dir, "update-ref", "-m", "mill: undo the squash for restack", "HEAD", oldHead, newHead); err != nil {
		return fmt.Errorf("move the branch back from the squashed commit %s to %s: %w; run `git reset --soft %s` in %s", newHead, oldHead, err, oldHead, dir)
	}
	return nil
}

// squashMessage is the message of a squashed restack commit, e.g.
// "mill: g1/x attempts 1-3, 5 (squashed for restack)".
func squashMessage(task string, attempts []int) string {
	var b strings.Builder
	switch len(attempts) {
	case 0:
		fmt.Fprintf(&b, "mill: %s own commits (squashed for restack)\n", task)
	case 1:
		fmt.Fprintf(&b, "mill: %s attempt %d (squashed for restack)\n", task, attempts[0])
	default:
		fmt.Fprintf(&b, "mill: %s attempts %s (squashed for restack)\n", task, attemptRanges(attempts))
	}
	fmt.Fprintf(&b, "\nMill-Task: %s\n", task)
	for _, n := range attempts {
		fmt.Fprintf(&b, "Mill-Attempt: %d\n", n)
	}
	return b.String()
}

// attemptRanges writes sorted, distinct attempt numbers as ranges: "1-3, 5".
func attemptRanges(ns []int) string {
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(ns[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// ReplayOnto replays the net change of the own commits (the commits after upstream) of the
// branch checked out in dir onto `onto`: SquashOwn, then RebaseOnto. On any failure the branch
// is back at its old head; a conflict is a *RebaseConflictError. It returns the new HEAD sha.
func ReplayOnto(ctx context.Context, dir, task, onto, upstream string) (string, error) {
	oldHead, squashed, err := SquashOwn(ctx, dir, task, upstream)
	if err != nil {
		return "", err
	}
	head, err := RebaseOnto(ctx, dir, onto, upstream)
	if err != nil {
		if uerr := Unsquash(ctx, dir, oldHead, squashed); uerr != nil {
			return "", errors.Join(err, uerr)
		}
		return "", err
	}
	return head, nil
}

// runExit runs git in dir and returns trimmed stdout and git's exit code. err is set only
// when git could not run or was killed (not for a non-zero exit).
func runExit(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd := Command(ctx, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started(ctx)
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() < 0 || ctx.Err() != nil) {
		failed(ctx, dir)
		return "", -1, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), withCtxErr(ctx, err), strings.TrimSpace(stderr.String()))
	}
	code := 0
	if ee != nil {
		code = ee.ExitCode()
	}
	if code != 0 && code != 1 {
		failed(ctx, dir)
		return "", code, fmt.Errorf("git %s: exit status %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), code, nil
}

// identity returns the -c options for a commit without hooks or signing, adding mill's own
// identity when the repository has none (or when that cannot be read: the commit then still
// has an author). The answer comes from ctx's Cache when it has one.
func identity(ctx context.Context, dir string) []string {
	args := []string{"-c", "commit.gpgsign=false"}
	if ok, err := hasIdentity(ctx, dir); err != nil || !ok {
		args = append(args, "-c", "user.name=mill", "-c", "user.email=mill@localhost")
	}
	return args
}

// hasIdentity reports whether the repository at dir has a non-empty user.email. An unset
// user.email (git config exit 1) is a valid answer; any other failure is an error. Answers
// are cached when ctx carries a Cache, and dropped when a git command with that ctx fails in dir.
func hasIdentity(ctx context.Context, dir string) (bool, error) {
	v, err := lookup(ctx, cacheKey{kind: "identity", dir: cacheDir(dir)}, func() (string, error) {
		out, code, err := runExit(ctx, dir, "config", "user.email")
		if err != nil {
			return "", err
		}
		if code == 1 || out == "" {
			return "no", nil
		}
		return "yes", nil
	})
	if err != nil {
		return false, err
	}
	return v == "yes", nil
}

// fetchLocks serializes fetches of the same repository within the process: two concurrent
// fetches updating the same remote-tracking refs can fail with "cannot lock ref".
var fetchLocks sync.Map // repository key -> *sync.Mutex

// repoLock returns the fetch mutex of the repository dir belongs to. Worktrees share their
// main repository's refs, so the key is the common git directory.
func repoLock(ctx context.Context, dir string) *sync.Mutex {
	key, err := CommonDir(ctx, dir)
	if err != nil {
		if key, err = filepath.Abs(dir); err != nil {
			key = dir
		}
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	m, _ := fetchLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Fetch fetches the branches of remote (its configured remote-tracking refs). Fetches of the
// same repository run one at a time.
func Fetch(ctx context.Context, dir, remote string) error {
	if strings.HasPrefix(remote, "-") || remote == "" {
		return fmt.Errorf("fetch: invalid remote %q", remote)
	}
	mu := repoLock(ctx, dir)
	mu.Lock()
	defer mu.Unlock()
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

// ApplyTree applies the change from `from` to `to` onto `onto` without touching any work tree
// or ref (a three-way merge with `from` as the merge base) and returns the resulting tree. On
// conflicts it returns no tree and the conflicted paths. When onto is from, the result is
// to's tree.
func ApplyTree(ctx context.Context, dir, from, to, onto string) (tree string, conflicts []string, err error) {
	if from == onto {
		tree, err := TreeOf(ctx, dir, to)
		if err != nil {
			return "", nil, fmt.Errorf("read the tree of %s: %w", to, err)
		}
		return tree, nil, nil
	}
	out, code, err := runExit(ctx, dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", "--merge-base="+from, onto, to)
	if err != nil {
		return "", nil, err
	}
	parts := strings.Split(out, "\x00")
	switch code {
	case 0:
		if parts[0] == "" {
			return "", nil, fmt.Errorf("git merge-tree --merge-base=%s %s %s wrote no tree", from, onto, to)
		}
		return parts[0], nil, nil
	case 1:
		for _, f := range parts[1:] {
			if f != "" {
				conflicts = append(conflicts, f)
			}
		}
		if len(conflicts) == 0 {
			return "", nil, fmt.Errorf("git merge-tree --merge-base=%s %s %s reported a conflict but listed no paths", from, onto, to)
		}
		return "", conflicts, nil
	}
	return "", nil, fmt.Errorf("git merge-tree --merge-base=%s %s %s failed (exit %d): %s", from, onto, to, code, strings.TrimSpace(out))
}

// ChangedPaths lists, sorted, the paths that differ between the trees of from and to, without
// rename detection (a rename is its old and its new path).
func ChangedPaths(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", "--no-renames", "--no-color", "--no-ext-diff", "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	sort.Strings(files)
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

// DeleteReview deletes refs/heads/<branch> on remote, only if it is still at expected. Like
// PushReview it refuses, without running git, any branch outside review/ and an empty expected sha.
func DeleteReview(ctx context.Context, dir, remote, branch, expected string) error {
	if !strings.HasPrefix(branch, ReviewPrefix) || len(branch) == len(ReviewPrefix) {
		return fmt.Errorf("refusing to delete branch %q: mill only deletes review/<name> branches", branch)
	}
	if remote == "" || strings.HasPrefix(remote, "-") || expected == "" || strings.HasPrefix(expected, "-") {
		return fmt.Errorf("refusing to delete %q on remote %q: invalid remote or expected sha", branch, remote)
	}
	ref := "refs/heads/" + branch
	_, err := Run(ctx, dir, "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+expected, remote, ":"+ref)
	return err
}

// RemoteBranchSHA returns the sha of refs/heads/<branch> on remote, or ok=false when the
// remote has no such branch.
func RemoteBranchSHA(ctx context.Context, dir, remote, branch string) (sha string, ok bool, err error) {
	out, err := Run(ctx, dir, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/"+branch {
			return f[0], true, nil
		}
	}
	return "", false, nil
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

// RebaseState is where a step-wise rebase (RebaseStart, RebaseContinue) stands.
type RebaseState struct {
	Done           bool     // the rebase finished
	Head           string   // the new HEAD sha, when Done
	Stopped        string   // REBASE_HEAD: the commit being replayed, when stopped
	Files          []string // the conflicted files, when stopped
	RerereResolved int      // stops that rerere resolved completely and that were continued automatically
}

// rebaseOpts returns the -c options of a rebase without hooks or signing (as RebaseOnto),
// plus rerere with autoupdate when rerere is set.
func rebaseOpts(ctx context.Context, dir string, rerere bool) []string {
	args := append(identity(ctx, dir), "-c", "core.hooksPath=/dev/null")
	if rerere {
		args = append(args, "-c", "rerere.enabled=true", "-c", "rerere.autoupdate=true")
	}
	return args
}

// RebaseStart replays the commits after `upstream` on the branch checked out in dir onto
// `onto`, like RebaseOnto, but it does not abort on conflicts: it returns the stop (REBASE_HEAD
// and the conflicted files) and leaves the rebase in progress for RebaseContinue or
// RebaseAbort. With rerere set, recorded resolutions are reused; a stop that rerere resolves
// completely is staged and continued automatically.
func RebaseStart(ctx context.Context, dir, onto, upstream string, rerere bool) (RebaseState, error) {
	args := append(rebaseOpts(ctx, dir, rerere), "rebase", "--no-verify", "--onto", onto, upstream)
	_, err := runEnv(ctx, dir, "", []string{"GIT_EDITOR=true"}, args...)
	return rebaseSteps(ctx, dir, rerere, err)
}

// RebaseContinue stages files (the resolved conflicts) and continues the rebase in progress in
// dir without opening an editor. It returns the next stop, or Done.
func RebaseContinue(ctx context.Context, dir string, files []string, rerere bool) (RebaseState, error) {
	in, err := rebaseInProgress(ctx, dir)
	if err != nil {
		return RebaseState{}, err
	}
	if !in {
		return RebaseState{}, fmt.Errorf("continue rebase: no rebase is in progress in %s", dir)
	}
	if err := addFiles(ctx, dir, files); err != nil {
		return RebaseState{}, fmt.Errorf("%w; fix the files and continue again, or run `git rebase --abort` in %s", err, dir)
	}
	return rebaseSteps(ctx, dir, rerere, rebaseContinue(ctx, dir, rerere))
}

// RebaseAbort aborts the rebase in progress in dir, restoring the original branch head.
func RebaseAbort(ctx context.Context, dir string) error {
	if _, err := Run(ctx, dir, "rebase", "--abort"); err != nil {
		return fmt.Errorf("aborting the rebase in %s: %w; inspect it with `git status` in %s", dir, err, dir)
	}
	return nil
}

func rebaseContinue(ctx context.Context, dir string, rerere bool) error {
	args := append(rebaseOpts(ctx, dir, rerere), "rebase", "--continue")
	_, err := runEnv(ctx, dir, "", []string{"GIT_EDITOR=true"}, args...)
	return err
}

// rebaseSteps reports the outcome of a rebase command that returned runErr, continuing by
// itself over stops that rerere resolved completely.
func rebaseSteps(ctx context.Context, dir string, rerere bool, runErr error) (RebaseState, error) {
	var st RebaseState
	last := ""
	for {
		in, err := rebaseInProgress(ctx, dir)
		if err != nil {
			return st, err
		}
		if !in {
			if runErr != nil {
				return st, fmt.Errorf("rebase in %s failed and is not in progress: %w", dir, runErr)
			}
			head, err := RevParse(ctx, dir, "HEAD")
			if err != nil {
				return st, fmt.Errorf("rebase in %s finished but HEAD cannot be read: %w", dir, err)
			}
			st.Done, st.Head = true, head
			return st, nil
		}
		stopped, err := RevParse(ctx, dir, "REBASE_HEAD")
		if err != nil {
			return st, fmt.Errorf("rebase in %s stopped but REBASE_HEAD cannot be read (%v): %w; run `git rebase --abort` in %s", dir, runErr, err, dir)
		}
		files, err := unmergedFiles(ctx, dir)
		if err != nil {
			return st, fmt.Errorf("%w; run `git rebase --abort` in %s", err, dir)
		}
		if len(files) > 0 {
			st.Stopped, st.Files = stopped, files
			return st, nil
		}
		if !rerere || stopped == last {
			return st, fmt.Errorf("rebase in %s stopped at %s without conflicted files: %v; resolve it by hand or run `git rebase --abort` in %s", dir, stopped, runErr, dir)
		}
		// rerere resolved and staged every conflicted file; continue unless markers remain.
		marked, err := rerereMarkedFiles(ctx, dir)
		if err != nil {
			return st, fmt.Errorf("%w; run `git rebase --abort` in %s", err, dir)
		}
		if len(marked) > 0 {
			st.Stopped, st.Files = stopped, marked
			return st, nil
		}
		st.RerereResolved++
		last = stopped
		runErr = rebaseContinue(ctx, dir, rerere)
	}
}

// rebaseInProgress reports whether a rebase is in progress in dir.
func rebaseInProgress(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		ok, err := gitPathExists(ctx, dir, name)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// gitPathExists reports whether the git-dir path name (git rev-parse --git-path) exists in dir.
func gitPathExists(ctx context.Context, dir, name string) (bool, error) {
	p, err := Run(ctx, dir, "rev-parse", "--git-path", name)
	if err != nil {
		return false, fmt.Errorf("locating %s in %s: %w", name, dir, err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if _, err := os.Stat(p); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("checking %s: %w", p, err)
	}
	return false, nil
}

// unmergedFiles lists the conflicted (unmerged) files in dir.
func unmergedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("listing conflicted files in %s: %w", dir, err)
	}
	return splitNUL(out), nil
}

// rerereMarkedFiles returns the files rerere is tracking for the current conflict that still
// contain conflict markers.
func rerereMarkedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := Run(ctx, dir, "-c", "rerere.enabled=true", "rerere", "status")
	if err != nil {
		return nil, fmt.Errorf("listing rerere's files in %s: %w", dir, err)
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return HasConflictMarkers(ctx, dir, files)
}

func splitNUL(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func addFiles(ctx context.Context, dir string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	_, err := Run(ctx, dir, append([]string{"add", "--"}, files...)...)
	return err
}

// MergeState is where a step-wise merge (MergeStart) stands.
type MergeState struct {
	Done           bool     // the merge commit was created
	Head           string   // the merge commit, when Done
	Files          []string // the conflicted files, when not Done
	RerereResolved bool     // rerere resolved every conflict and the merge was committed
}

// MergeStart merges branch into the branch checked out in dir with a merge commit, like Merge,
// but it leaves conflicts in place for MergeContinue or MergeAbort and reports the conflicted
// files. With rerere set, recorded resolutions are reused; when rerere resolves every conflict
// the merge is committed.
func MergeStart(ctx context.Context, dir, branch, message string, rerere bool) (MergeState, error) {
	args := identity(ctx, dir)
	if rerere {
		args = append(args, "-c", "rerere.enabled=true", "-c", "rerere.autoupdate=true")
	}
	args = append(args, "merge", "--no-ff", "--no-verify", "-q", "-m", message, branch)
	_, mergeErr := Run(ctx, dir, args...)
	if mergeErr == nil {
		head, err := RevParse(ctx, dir, "HEAD")
		if err != nil {
			return MergeState{}, fmt.Errorf("merge in %s finished but HEAD cannot be read: %w", dir, err)
		}
		return MergeState{Done: true, Head: head}, nil
	}
	in, err := gitPathExists(ctx, dir, "MERGE_HEAD")
	if err != nil {
		return MergeState{}, fmt.Errorf("%w; and %v", mergeErr, err)
	}
	if !in {
		return MergeState{}, fmt.Errorf("merge in %s failed and is not in progress: %w", dir, mergeErr)
	}
	files, err := unmergedFiles(ctx, dir)
	if err != nil {
		return MergeState{}, fmt.Errorf("%w; run `git merge --abort` in %s", err, dir)
	}
	if len(files) > 0 {
		return MergeState{Files: files}, nil
	}
	if !rerere {
		return MergeState{}, fmt.Errorf("merge in %s stopped without conflicted files: %v; run `git merge --abort` in %s", dir, mergeErr, dir)
	}
	marked, err := rerereMarkedFiles(ctx, dir)
	if err != nil {
		return MergeState{}, fmt.Errorf("%w; run `git merge --abort` in %s", err, dir)
	}
	if len(marked) > 0 {
		return MergeState{Files: marked}, nil
	}
	sha, err := MergeContinue(ctx, dir, nil, message)
	if err != nil {
		return MergeState{}, err
	}
	return MergeState{Done: true, Head: sha, RerereResolved: true}, nil
}

// MergeContinue stages files (the resolved conflicts) and commits the merge in progress in dir
// with message, skipping hooks and signing. It returns the merge commit.
func MergeContinue(ctx context.Context, dir string, files []string, message string) (string, error) {
	in, err := gitPathExists(ctx, dir, "MERGE_HEAD")
	if err != nil {
		return "", err
	}
	if !in {
		return "", fmt.Errorf("continue merge: no merge is in progress in %s", dir)
	}
	if err := addFiles(ctx, dir, files); err != nil {
		return "", fmt.Errorf("%w; fix the files and continue again, or run `git merge --abort` in %s", err, dir)
	}
	args := identity(ctx, dir)
	// Let rerere record the resolution when it saw the conflict.
	if rr, err := gitPathExists(ctx, dir, "MERGE_RR"); err != nil {
		return "", err
	} else if rr {
		args = append(args, "-c", "rerere.enabled=true")
	}
	args = append(args, "commit", "-q", "--no-verify", "-F", "-")
	if _, err := RunInput(ctx, dir, message, args...); err != nil {
		return "", fmt.Errorf("committing the merge in %s: %w; fix it and continue again, or run `git merge --abort` in %s", dir, err, dir)
	}
	return RevParse(ctx, dir, "HEAD")
}

// MergeAbort aborts the merge in progress in dir, restoring the original head.
func MergeAbort(ctx context.Context, dir string) error {
	if _, err := Run(ctx, dir, "merge", "--abort"); err != nil {
		return fmt.Errorf("aborting the merge in %s: %w; inspect it with `git status` in %s", dir, err, dir)
	}
	return nil
}

// hunkContext is the number of lines ConflictHunks shows around each conflict.
const hunkContext = 3

// isMarker reports whether line is a conflict marker line: <<<<<<<, ======= or >>>>>>>.
func isMarker(line string) bool {
	line = strings.TrimRight(line, "\r")
	if line == "=======" {
		return true
	}
	for _, m := range []string{"<<<<<<<", ">>>>>>>"} {
		if line == m || strings.HasPrefix(line, m+" ") {
			return true
		}
	}
	return false
}

// ConflictHunks returns the conflict regions of file (relative to dir), from <<<<<<< to
// >>>>>>> with a few lines of context, each headed by its line range. It returns "" when the
// file has no conflict.
func ConflictHunks(ctx context.Context, dir, file string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "", fmt.Errorf("reading conflicted file: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	type span struct{ from, to int } // inclusive line indexes
	var spans []span
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "<<<<<<<") || !isMarker(lines[i]) {
			continue
		}
		end := i
		for end < len(lines) && !(strings.HasPrefix(lines[end], ">>>>>>>") && isMarker(lines[end])) {
			end++
		}
		if end == len(lines) {
			end = len(lines) - 1 // unterminated: show to the end
		}
		s := span{max(0, i-hunkContext), min(len(lines)-1, end+hunkContext)}
		if n := len(spans); n > 0 && s.from <= spans[n-1].to+1 {
			spans[n-1].to = s.to
		} else {
			spans = append(spans, s)
		}
		i = end
	}
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "@@ %s lines %d-%d @@\n", file, s.from+1, s.to+1)
		for _, l := range lines[s.from : s.to+1] {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// HasConflictMarkers returns the files (relative to dir) that still contain a <<<<<<<,
// ======= or >>>>>>> marker line. A file that no longer exists has none.
func HasConflictMarkers(ctx context.Context, dir string, files []string) ([]string, error) {
	var marked []string
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking %s for conflict markers: %w", f, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if isMarker(line) {
				marked = append(marked, f)
				break
			}
		}
	}
	return marked, nil
}

// CommitSummary is one commit's sha, subject and trailers ("Key: value" lines).
type CommitSummary struct {
	SHA      string
	Subject  string
	Trailers []string
}

// CommitSummaries returns the commits in from..to, oldest first, with their subjects and
// trailers (git interpret-trailers --parse).
func CommitSummaries(ctx context.Context, dir, from, to string) ([]CommitSummary, error) {
	out, err := Run(ctx, dir, "rev-list", "--reverse", from+".."+to, "--")
	if err != nil {
		return nil, fmt.Errorf("listing commits %s..%s in %s: %w", from, to, dir, err)
	}
	var sums []CommitSummary
	for _, sha := range strings.Fields(out) {
		msg, err := Run(ctx, dir, "show", "-s", "--format=%s%x00%B", sha)
		if err != nil {
			return nil, fmt.Errorf("reading commit %s in %s: %w", sha, dir, err)
		}
		subject, body, _ := strings.Cut(msg, "\x00")
		s := CommitSummary{SHA: sha, Subject: subject}
		trailers, err := RunInput(ctx, dir, body+"\n", "interpret-trailers", "--parse")
		if err != nil {
			return nil, fmt.Errorf("parsing the trailers of %s in %s: %w", sha, dir, err)
		}
		for _, t := range strings.Split(trailers, "\n") {
			if t != "" {
				s.Trailers = append(s.Trailers, t)
			}
		}
		sums = append(sums, s)
	}
	return sums, nil
}
