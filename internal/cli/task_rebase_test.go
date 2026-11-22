package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mill/internal/gitx"
	"mill/internal/store"
)

const chainPlan = `{
  "goal": "Chain",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": ["a"], "size": "S"}
  ]
}`

func rebaseScript(soloFiles string) string {
	return `{"tasks": {
  "solo": ` + taskSteps(soloFiles) + `,
  "g1/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g1/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `,
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `,
  "g2/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g2/b": ` + taskSteps(`{"b.txt": "b\n"}`) + `
}}`
}

func gitC(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func commitOnMain(t *testing.T, root, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, root, "add", name)
	gitC(t, root, "commit", "-q", "-m", "main: "+name)
	return gitC(t, root, "rev-parse", "HEAD")
}

// soloRepo has a verified standalone task "solo" and the task file for it.
func soloRepo(t *testing.T, soloFiles string) (root string) {
	t.Helper()
	root = goalRepo(t, rebaseScript(soloFiles), "")
	spec := `{"id": "solo", "title": "Solo", "kind": "implement", "spec": "write", "acceptance": ["ok"],
  "checks": ["true"], "owns": [], "depends_on": [], "size": "S"}`
	if r := run(t, root, nil, "task", "run", "--file", writeFile(t, spec)); r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	setStatus(t, root, "solo", store.TaskBlocked)
	return root
}

func TestTaskRebaseMovesOwnCommitsOntoMovedBase(t *testing.T) {
	root := soloRepo(t, `{"solo.txt": "s\n"}`)
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "solo")
	tip := commitOnMain(t, root, "m.txt", "m\n")

	r := run(t, root, nil, "task", "rebase", "solo")
	if r.code != ExitOK || !strings.Contains(r.stdout, short(before.StartSHA)) || !strings.Contains(r.stdout, short(tip)) ||
		!strings.Contains(r.stdout, "mill task run --file <file> --retry") {
		t.Fatalf("rebase: %+v", r)
	}
	after, err := s.GetBranch(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	head := gitC(t, after.Worktree, "rev-parse", "HEAD")
	if after.BaseSHA != tip || after.StartSHA != tip || after.HeadSHA != head || head == before.HeadSHA {
		t.Errorf("branch row = %+v, tip %s, head %s", after, tip, head)
	}
	if parent := gitC(t, after.Worktree, "rev-parse", "HEAD~1"); parent != tip {
		t.Errorf("own commit's parent = %s, want %s", parent, tip)
	}
	for _, f := range []string{"m.txt", "solo.txt"} {
		if _, err := os.Stat(filepath.Join(after.Worktree, f)); err != nil {
			t.Errorf("worktree lacks %s", f)
		}
	}
	evs, _ := s.TaskEvents(ctx, "solo")
	found := false
	for _, e := range evs {
		found = found || (e.Kind == "task-rebased" && strings.Contains(string(e.Data), before.StartSHA) && strings.Contains(string(e.Data), tip))
	}
	if !found {
		t.Errorf("no task-rebased event: %+v", evs)
	}

	r = run(t, root, nil, "task", "rebase", "solo")
	if r.code != ExitOK || !strings.Contains(r.stdout, "already up to date") {
		t.Fatalf("second rebase: %+v", r)
	}
	again, _ := s.GetBranch(ctx, "solo")
	if again.HeadSHA != after.HeadSHA || again.StartSHA != after.StartSHA {
		t.Errorf("up-to-date rebase changed the row: %+v", again)
	}
}

func TestTaskRebaseConflictLeavesBranchUnchanged(t *testing.T) {
	root := soloRepo(t, `{"clash.txt": "task\n"}`)
	ctx := context.Background()
	s := openStore(t, root)
	before, _ := s.GetBranch(ctx, "solo")
	commitOnMain(t, root, "clash.txt", "main\n")

	r := run(t, root, nil, "task", "rebase", "solo")
	if r.code != ExitError || !strings.Contains(r.stderr, "clash.txt") || !strings.Contains(r.stderr, "aborted") {
		t.Fatalf("rebase: %+v", r)
	}
	after, _ := s.GetBranch(ctx, "solo")
	if after != before && (after.HeadSHA != before.HeadSHA || after.StartSHA != before.StartSHA || after.BaseSHA != before.BaseSHA) {
		t.Errorf("row changed: %+v -> %+v", before, after)
	}
	if head := gitC(t, before.Worktree, "rev-parse", "HEAD"); head != before.HeadSHA {
		t.Errorf("head = %s, want %s", head, before.HeadSHA)
	}
	if st := gitC(t, before.Worktree, "status", "--porcelain"); st != "" {
		t.Errorf("worktree not clean: %s", st)
	}
}

func TestTaskRebaseRefusals(t *testing.T) {
	root := soloRepo(t, `{"solo.txt": "s\n"}`)
	ctx := context.Background()
	s := openStore(t, root)
	br, _ := s.GetBranch(ctx, "solo")
	commitOnMain(t, root, "m.txt", "m\n")

	for _, st := range []string{store.TaskRunning, store.TaskVerified} {
		setStatus(t, root, "solo", st)
		if r := run(t, root, nil, "task", "rebase", "solo"); r.code != ExitError || !strings.Contains(r.stderr, st) {
			t.Errorf("%s: %+v", st, r)
		}
	}
	setStatus(t, root, "solo", store.TaskBlocked)

	if err := os.WriteFile(filepath.Join(br.Worktree, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "task", "rebase", "solo"); r.code != ExitError || !strings.Contains(r.stderr, "uncommitted changes") {
		t.Errorf("dirty: %+v", r)
	}
	if after, _ := s.GetBranch(ctx, "solo"); after.StartSHA != br.StartSHA {
		t.Errorf("dirty refusal changed the row: %+v", after)
	}

	// A pending goal task that never ran has no branch.
	if r := run(t, root, nil, "task", "rebase", "g1/a"); r.code != ExitError || !strings.Contains(r.stderr, "no branch") {
		t.Errorf("no branch: %+v", r)
	}
}

func TestTaskRebaseGoalTaskOntoDependencyHead(t *testing.T) {
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, chainPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	if r := run(t, root, nil, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	ctx := context.Background()
	s := openStore(t, root)
	b, _ := s.GetBranch(ctx, "g2/b")
	a, _ := s.GetBranch(ctx, "g2/a")
	if b.StartSHA != a.HeadSHA {
		t.Fatalf("b starts at %s, a head %s", b.StartSHA, a.HeadSHA)
	}
	// Move a's branch forward by a commit made in its worktree.
	if err := os.WriteFile(filepath.Join(a.Worktree, "extra.txt"), []byte("e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, a.Worktree, "add", "extra.txt")
	gitC(t, a.Worktree, "commit", "-q", "-m", "extra")
	aTip := gitC(t, a.Worktree, "rev-parse", "HEAD")
	setStatus(t, root, "g2/b", store.TaskBlocked)

	r := run(t, root, nil, "task", "rebase", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "mill run g2 --retry-blocked") {
		t.Fatalf("rebase: %+v", r)
	}
	after, _ := s.GetBranch(ctx, "g2/b")
	if after.StartSHA != aTip || after.BaseSHA != aTip || gitC(t, after.Worktree, "rev-parse", "HEAD~1") != aTip {
		t.Errorf("row = %+v, a tip %s", after, aTip)
	}
	if _, err := os.Stat(filepath.Join(after.Worktree, "extra.txt")); err != nil {
		t.Errorf("rebased worktree lacks extra.txt")
	}
}

func TestTaskRebaseRefusesFanInTask(t *testing.T) {
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	if r := run(t, root, nil, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	setStatus(t, root, "g1/c", store.TaskBlocked)
	r := run(t, root, nil, "task", "rebase", "g1/c")
	if r.code != ExitError || !strings.Contains(r.stderr, "rebasing fan-in tasks is not supported yet") {
		t.Fatalf("rebase: %+v", r)
	}
}
