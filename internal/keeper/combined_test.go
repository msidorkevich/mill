package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

type combinedEnv struct {
	o      Options
	out    *bytes.Buffer
	root   string
	wtRoot string
	goal   string
	ids    []string
}

// combinedSetup creates a repo with a bare remote and one goal whose independent tasks are
// verified; each task's branch adds <task>.txt with the given content (default "x"), and its
// gate ran at its head. checkCmd is the gate.
func combinedSetup(t *testing.T, checkCmd string, locals []string, content map[string]string) *combinedEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".mill/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".gitignore")
	base := writeCommit(t, root, "shared.txt", original, "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")
	git(t, root, "fetch", "-q", "origin")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.Runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pl := plan.Plan{Goal: "combine"}
	for _, l := range locals {
		pl.Tasks = append(pl.Tasks, taskspec.Spec{ID: l, Title: "Task " + l, Kind: "implement", Spec: "add " + l,
			Acceptance: []string{l + " exists"}, Owns: []string{}, Size: "S"})
	}
	raw, _ := json.Marshal(pl)
	g, err := s.CreateGoal(ctx, "combine", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	e := &combinedEnv{root: root, goal: g.ID, wtRoot: t.TempDir(), out: &bytes.Buffer{}}
	for _, l := range locals {
		id := store.TaskID(g.ID, l)
		wt := filepath.Join(t.TempDir(), l)
		name := "mill/" + id
		git(t, root, "worktree", "add", "-q", "-b", name, wt, base)
		c, ok := content[l]
		if !ok {
			c = "x\n"
		}
		head := writeCommit(t, wt, l+".txt", c, "add "+l)
		if err := s.SaveBranch(ctx, store.Branch{TaskID: id, Name: name, Base: "main", BaseSHA: base, StartSHA: base,
			HeadSHA: head, GateSHA: head, Worktree: wt}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, id, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
		e.ids = append(e.ids, id)
	}

	cfg := config.Default()
	cfg.CheckCmd = checkCmd
	cfg.WorktreeRoot = e.wtRoot
	cfg.GateTimeout = config.Duration{Duration: 10 * time.Second}
	e.o = Options{Store: s, Cfg: cfg, Paths: paths, Host: host.NewExec(), Out: e.out}
	return e
}

// assertNoThrowaway checks no combined-check worktree is left on disk or in git's list.
func (e *combinedEnv) assertNoThrowaway(t *testing.T) {
	t.Helper()
	list := git(t, e.root, "worktree", "list", "--porcelain")
	if strings.Contains(list, ".combined-") {
		t.Fatalf("throwaway worktree still listed:\n%s", list)
	}
	dir := e.o.Cfg.Worktrees(e.root, "")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".combined-") {
			t.Fatalf("throwaway worktree %s left in %s", en.Name(), dir)
		}
	}
}

func (e *combinedEnv) events(t *testing.T, kind string) []store.Event {
	t.Helper()
	var out []store.Event
	for _, id := range e.ids {
		evs, err := e.o.Store.TaskEvents(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == kind {
				out = append(out, ev)
			}
		}
	}
	return out
}

func TestCombinedFindsSemanticConflictPair(t *testing.T) {
	// Each branch alone is green; a and c together are red.
	e := combinedSetup(t, "! { test -f a.txt && test -f c.txt; }", []string{"a", "b", "c", "d"}, nil)
	ctx := context.Background()
	res, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatalf("Combined: %v\nlog:\n%s", err, e.out)
	}
	a, c := e.ids[0], e.ids[2]
	if !res.Ran || res.Passed || res.Culprit != c || res.Partner != a {
		t.Fatalf("result = %+v, want culprit %s partner %s\nlog:\n%s", res, c, a, e.out)
	}
	if strings.Join(res.Branches, " ") != strings.Join(e.ids, " ") {
		t.Fatalf("branches = %v, want %v", res.Branches, e.ids)
	}
	if !strings.Contains(res.Failed, "test -f a.txt") || res.Probes < 3 {
		t.Fatalf("failed = %q, probes = %d", res.Failed, res.Probes)
	}
	evs := e.events(t, "combined-conflict")
	if len(evs) != 1 {
		t.Fatalf("combined-conflict events = %d, want 1", len(evs))
	}
	var data struct {
		Tasks []string `json:"tasks"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Tasks) != 2 || !strings.Contains(strings.Join(data.Tasks, " "), a) || !strings.Contains(strings.Join(data.Tasks, " "), c) {
		t.Fatalf("event tasks = %v, want %s and %s", data.Tasks, a, c)
	}
	for _, id := range e.ids {
		task, err := e.o.Store.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != store.TaskVerified {
			t.Fatalf("%s status = %s, want verified", id, task.Status)
		}
		paired := id == a || id == c
		if paired != strings.Contains(task.Reason, "combined check") {
			t.Fatalf("%s reason = %q (paired %v)", id, task.Reason, paired)
		}
	}
	e.assertNoThrowaway(t)
}

func TestCombinedFindsSemanticConflictBetweenTwoBranches(t *testing.T) {
	// Each branch is green with the gate alone; merged together they are red.
	e := combinedSetup(t, "! { test -f a.txt && test -f b.txt; }", []string{"a", "b"}, nil)
	ctx := context.Background()
	res, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatalf("Combined: %v\nlog:\n%s", err, e.out)
	}
	a, b := e.ids[0], e.ids[1]
	if !res.Ran || res.Passed || res.Culprit != b || res.Partner != a {
		t.Fatalf("result = %+v, want culprit %s partner %s\nlog:\n%s", res, b, a, e.out)
	}
	evs := e.events(t, "combined-conflict")
	if len(evs) != 1 {
		t.Fatalf("combined-conflict events = %d, want 1", len(evs))
	}
	var data struct {
		Tasks []string `json:"tasks"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Join(data.Tasks, " ") != a+" "+b {
		t.Fatalf("event tasks = %v, want %s and %s", data.Tasks, a, b)
	}
	for _, id := range e.ids {
		task, err := e.o.Store.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != store.TaskVerified || !strings.Contains(task.Reason, "combined check") {
			t.Fatalf("%s status = %s reason = %q, want verified with a combined-check reason", id, task.Status, task.Reason)
		}
	}
	e.assertNoThrowaway(t)
}

func TestCombinedTreatsMergeConflictAsRed(t *testing.T) {
	e := combinedSetup(t, "true", []string{"a", "b"}, nil)
	ctx := context.Background()
	// Both branches also edit shared.txt differently.
	for i, side := range []string{taskSide, mainSide} {
		br, err := e.o.Store.GetBranch(ctx, e.ids[i])
		if err != nil {
			t.Fatal(err)
		}
		br.HeadSHA = writeCommit(t, br.Worktree, "shared.txt", side, "edit shared")
		br.GateSHA = br.HeadSHA
		if err := e.o.Store.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatalf("Combined: %v\nlog:\n%s", err, e.out)
	}
	if res.Passed || res.Culprit != e.ids[1] || res.Partner != e.ids[0] || !strings.Contains(res.Failed, "merge conflict") {
		t.Fatalf("result = %+v", res)
	}
	e.assertNoThrowaway(t)
}

func TestCombinedPassesCompatibleBranches(t *testing.T) {
	e := combinedSetup(t, "test -f shared.txt", []string{"a", "b", "c"}, nil)
	res, err := Combined(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Combined: %v\nlog:\n%s", err, e.out)
	}
	if !res.Ran || !res.Passed || res.Culprit != "" || res.Probes != 1 {
		t.Fatalf("result = %+v", res)
	}
	if evs := e.events(t, "combined-conflict"); len(evs) != 0 {
		t.Fatalf("combined-conflict events = %d, want none", len(evs))
	}
	e.assertNoThrowaway(t)
}

func TestCombinedReportsCulpritRedWithBaseAlone(t *testing.T) {
	e := combinedSetup(t, "! test -f b.txt", []string{"a", "b"}, nil)
	res, err := Combined(context.Background(), e.o)
	if err != nil {
		t.Fatalf("Combined: %v\nlog:\n%s", err, e.out)
	}
	if res.Passed || res.Culprit != e.ids[1] || res.Partner != "" || res.Failed == "" {
		t.Fatalf("result = %+v", res)
	}
	if evs := e.events(t, "combined-conflict"); len(evs) != 0 {
		t.Fatalf("combined-conflict events = %d, want none", len(evs))
	}
	e.assertNoThrowaway(t)
}

func TestCombinedDoesNotRunWithFewerThanTwoReadyBranches(t *testing.T) {
	e := combinedSetup(t, "false", []string{"a", "b"}, nil)
	ctx := context.Background()
	// b's gate did not run at its head, so it is not ready.
	br, err := e.o.Store.GetBranch(ctx, e.ids[1])
	if err != nil {
		t.Fatal(err)
	}
	br.GateSHA = ""
	if err := e.o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	res, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatalf("Combined: %v", err)
	}
	if res.Ran || res.Passed || res.Probes != 0 || len(res.Branches) != 1 {
		t.Fatalf("result = %+v, want not run", res)
	}
	e.assertNoThrowaway(t)
}

// failingHost cannot start any process; it cancels cancel first when set.
type failingHost struct{ cancel context.CancelFunc }

func (h failingHost) Name() string { return "failing" }
func (h failingHost) Start(ctx context.Context, spec host.ProcSpec) (host.Handle, error) {
	if h.cancel != nil {
		h.cancel()
	}
	return nil, errors.New("no such host")
}

func TestCombinedFailsWhenGateCannotRun(t *testing.T) {
	e := combinedSetup(t, "true", []string{"a", "b"}, nil)
	e.o.Host = failingHost{}
	res, err := Combined(context.Background(), e.o)
	if err == nil || res.Passed {
		t.Fatalf("result = %+v, err = %v; want an error", res, err)
	}
	if !strings.Contains(err.Error(), "could not run") {
		t.Fatalf("err = %v", err)
	}
	e.assertNoThrowaway(t)
}

func TestCombinedFailsWithoutChecks(t *testing.T) {
	e := combinedSetup(t, "", []string{"a", "b"}, nil)
	res, err := Combined(context.Background(), e.o)
	if err == nil || res.Passed {
		t.Fatalf("result = %+v, err = %v; want an error", res, err)
	}
	e.assertNoThrowaway(t)
}

func TestCombinedRemovesWorktreeOnCancellation(t *testing.T) {
	e := combinedSetup(t, "true", []string{"a", "b"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.o.Host = failingHost{cancel: cancel}
	res, err := Combined(ctx, e.o)
	if err == nil || res.Passed {
		t.Fatalf("result = %+v, err = %v; want an error", res, err)
	}
	e.assertNoThrowaway(t)
}
