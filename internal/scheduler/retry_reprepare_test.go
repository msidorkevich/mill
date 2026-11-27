package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

// repo is a temporary git repository with a commit on main.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("commit", "-q", "--allow-empty", "-m", "base")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := gitx.Run(context.Background(), r.dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// commit adds a commit writing file on branch and returns its sha.
func (r *repo) commit(branch, file string) string {
	r.t.Helper()
	r.git("checkout", "-q", branch)
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(file+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", file)
	r.git("commit", "-q", "-m", file)
	return r.git("rev-parse", "HEAD")
}

// branch creates branch at start with one commit of its own and records it for task id.
func (r *repo) branch(s *store.Store, id, name, start string) {
	r.t.Helper()
	r.git("branch", name, start)
	head := r.commit(name, strings.ReplaceAll(name, "/", "-")+".txt")
	r.git("checkout", "-q", "main")
	if err := s.SaveBranch(context.Background(), store.Branch{TaskID: id, Name: name, Base: "main", BaseSHA: start, StartSHA: start, HeadSHA: head, Worktree: r.dir}); err != nil {
		r.t.Fatal(err)
	}
}

// recorder is a Reprepare callback that records the tasks it is called for.
type recorder struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (rc *recorder) reprepare(ctx context.Context, t store.Task) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.ids = append(rc.ids, t.ID)
	return rc.err
}

func (rc *recorder) called() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.ids)
}

func setStatuses(t *testing.T, s *store.Store, status string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.SetTaskStatus(context.Background(), id, status, "", "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetryBlockedReprepareOnlyTasksWhoseDependencyMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, g := newGoal(t, task{id: "a"}, task{id: "fresh", deps: []string{"a"}}, task{id: "stale", deps: []string{"a"}})
	a, fresh, stale := store.TaskID(g, "a"), store.TaskID(g, "fresh"), store.TaskID(g, "stale")
	r := newRepo(t)
	base := r.git("rev-parse", "main")
	r.branch(s, a, "mill/a", base)
	aOld := r.git("rev-parse", "mill/a")
	r.branch(s, stale, "mill/stale", aOld)
	aNew := r.commit("mill/a", "a2.txt")
	r.git("checkout", "-q", "main")
	r.branch(s, fresh, "mill/fresh", aNew)
	setStatuses(t, s, store.TaskVerified, a)
	setStatuses(t, s, store.TaskBlocked, fresh, stale)

	rc := &recorder{}
	out := &syncBuf{}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: pollSleep, Out: out}
	res, err := sch.RunGoal(ctx, g, Options{RetryBlocked: true, Reprepare: rc.reprepare, RepoDir: r.dir, BaseBranch: "main"})
	if err != nil || res.Status != store.GoalDone {
		t.Fatalf("retry run = %+v, %v\n%s", res, err, out.String())
	}
	if got := rc.called(); !slices.Equal(got, []string{stale}) {
		t.Errorf("Reprepare called for %v, want only %s", got, stale)
	}
	if !strings.Contains(out.String(), stale+": dependencies changed since preparation; rebased") {
		t.Errorf("log lacks the rebase line:\n%s", out.String())
	}
	if strings.Contains(out.String(), fresh+": dependencies changed") {
		t.Errorf("log says the unchanged task was rebased:\n%s", out.String())
	}
}

func TestRetryBlockedReprepareTaskWithoutDependenciesWhenBaseMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, g := newGoal(t, task{id: "old"}, task{id: "current"}, task{id: "nobranch"})
	old, current, nobranch := store.TaskID(g, "old"), store.TaskID(g, "current"), store.TaskID(g, "nobranch")
	r := newRepo(t)
	r.branch(s, old, "mill/old", r.git("rev-parse", "main"))
	r.branch(s, current, "mill/current", r.commit("main", "base2.txt"))
	setStatuses(t, s, store.TaskBlocked, old, nobranch)
	setStatuses(t, s, store.TaskFailed, current)

	rc := &recorder{}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 3, RunTask: f.run, Sleep: pollSleep}
	res, err := sch.RunGoal(ctx, g, Options{RetryBlocked: true, Reprepare: rc.reprepare, RepoDir: r.dir, BaseBranch: "main"})
	if err != nil || res.Status != store.GoalDone {
		t.Fatalf("retry run = %+v, %v", res, err)
	}
	if got := rc.called(); !slices.Equal(got, []string{old}) {
		t.Errorf("Reprepare called for %v, want only %s", got, old)
	}
	for _, id := range []string{old, current, nobranch} {
		if f.index("start "+id) < 0 {
			t.Errorf("%s did not run", id)
		}
	}
}

func TestRetryBlockedReprepareFailureLeavesTaskBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, g := newGoal(t, task{id: "a"}, task{id: "b", deps: []string{"a"}})
	a, b := store.TaskID(g, "a"), store.TaskID(g, "b")
	r := newRepo(t)
	r.branch(s, a, "mill/a", r.git("rev-parse", "main"))
	r.branch(s, b, "mill/b", r.git("rev-parse", "mill/a"))
	r.commit("mill/a", "a2.txt")
	r.git("checkout", "-q", "main")
	setStatuses(t, s, store.TaskVerified, a)
	setStatuses(t, s, store.TaskBlocked, b)

	rc := &recorder{err: errors.New("moving b onto mill/a conflicts in b.txt")}
	f := newFake(s)
	sch := &Scheduler{Store: s, MaxParallel: 2, RunTask: f.run, Sleep: pollSleep}
	res, err := sch.RunGoal(ctx, g, Options{RetryBlocked: true, Reprepare: rc.reprepare, RepoDir: r.dir, BaseBranch: "main"})
	if err != nil || res.Status != store.GoalBlocked {
		t.Fatalf("retry run = %+v, %v", res, err)
	}
	if f.index("start "+b) >= 0 {
		t.Errorf("%s started on its stale starting point", b)
	}
	tk := status(t, s, b)
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "conflicts in b.txt") || !strings.Contains(tk.Reason, "mill task rebase "+b) {
		t.Errorf("%s = %s (%s), want blocked with the failure and `mill task rebase %s`", b, tk.Status, tk.Reason, b)
	}
}
