package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fixture is a repo with a bare remote and a verified single task "t1" on branch mill/t/t1.
func fixture(t *testing.T) (Options, string, store.Branch) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README")
	git(t, root, "commit", "-q", "-m", "init")
	base := git(t, root, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")

	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", "-b", "mill/t/t1", wt, base)
	if err := os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "x.txt")
	git(t, wt, "commit", "-q", "-m", "work 1")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "work 2")
	head := git(t, wt, "rev-parse", "HEAD")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	spec := taskspec.Spec{ID: "t1", Title: "Write x", Kind: "implement", Spec: "write x", Acceptance: []string{"x exists"},
		Checks: []string{"test -f x.txt"}, Size: "S"}
	if err := s.CreateTask(ctx, spec); err != nil {
		t.Fatal(err)
	}
	br := store.Branch{TaskID: "t1", Name: "mill/t/t1", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: head, GateSHA: head, Worktree: wt}
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "task-verdict", "", "t1", map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "t1", store.TaskVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	return Options{Store: s, Cfg: cfg, Paths: paths, TaskID: "t1"}, bare, br
}

func TestPublishPushesSquashedCommitOnBase(t *testing.T) {
	t.Parallel()
	o, bare, br := fixture(t)
	mainBefore := git(t, bare, "rev-parse", "refs/heads/main")
	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "review/t1" || res.Parent != br.BaseSHA || res.Onto != "main" || res.Republish || res.GateRerun != nil {
		t.Errorf("result = %+v", res)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/review/t1"); got != res.SHA {
		t.Errorf("remote review/t1 = %s, want %s", got, res.SHA)
	}
	if got := git(t, bare, "rev-list", "--count", br.BaseSHA+".."+res.SHA); got != "1" {
		t.Errorf("%s commits on top of the base, want 1", got)
	}
	msg := git(t, bare, "log", "-1", "--format=%B", res.SHA)
	if !strings.HasPrefix(msg, "Write x\n") || strings.Contains(msg, "Mill-Goal") || !strings.Contains(msg, "Mill-Task: t1") ||
		!strings.Contains(msg, "Mill-Base: "+br.BaseSHA) || !strings.Contains(msg, "Verifier: pass: fine") || strings.Contains(msg, "mill review show") {
		t.Errorf("message:\n%s", msg)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Errorf("remote main moved to %s", got)
	}
	tk, _ := o.Store.GetTask(context.Background(), "t1")
	after, _ := o.Store.GetBranch(context.Background(), "t1")
	if tk.Status != store.TaskPublished || after.PublishedRef != "review/t1" || after.PublishedSHA != res.SHA {
		t.Errorf("task %s, branch %+v", tk.Status, after)
	}
}

func TestPublishRefusalsAreRefusedErrors(t *testing.T) {
	t.Parallel()
	o, bare, br := fixture(t)
	ctx := context.Background()

	br.GateSHA = ""
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	o.Cfg.CheckCmd = "false"
	res, err := Publish(ctx, o)
	var re *RefusedError
	if !errors.As(err, &re) || re.Rule != ruleGate || !strings.Contains(err.Error(), "false (exit 1") {
		t.Fatalf("gate refusal = %v", err)
	}
	if res.GateRerun == nil || res.GateRerun.Passed || len(res.GateRerun.Commands) != 2 {
		t.Errorf("gate re-run = %+v", res.GateRerun)
	}
	evs, _ := o.Store.TaskEvents(ctx, "t1")
	if last := evs[len(evs)-1]; last.Kind != "task-gate" {
		t.Errorf("last event = %+v, want the gate re-run", last)
	}
	if _, err := gitx.RevParse(ctx, bare, "refs/heads/review/t1"); err == nil {
		t.Error("refused publish pushed review/t1")
	}

	o.Cfg.Remote = "nowhere"
	if _, err := Publish(ctx, o); !errors.As(err, &re) || re.Rule != ruleRemote {
		t.Fatalf("remote refusal = %v", err)
	}
}

func TestParentRefusalRecognisesOnlyTheDependencyRule(t *testing.T) {
	t.Parallel()
	parentErr := fmt.Errorf("wrapped: %w", &RefusedError{Rule: ruleParent, Parent: "g/a", Msg: "publish g/a first"})
	if p, ok := ParentRefusal(parentErr); !ok || p != "g/a" {
		t.Errorf("ParentRefusal(parent rule) = %q, %v", p, ok)
	}
	for _, err := range []error{&RefusedError{Rule: ruleGate, Msg: "red"}, errors.New("push failed"), nil} {
		if _, ok := ParentRefusal(err); ok {
			t.Errorf("ParentRefusal(%v) = true", err)
		}
	}
}

func TestPublishMessageChecksAcceptanceAndTrimsLongSummary(t *testing.T) {
	t.Parallel()
	const pointer = "Full verdict: mill review show t1\n"
	// The 600-character limit applies to the summary alone, not the "pass: " prefix.
	cases := []struct {
		name, summary, want string
	}{
		{
			// Character 600 ends exactly on the end of a word.
			name:    "cut after a whole word",
			summary: strings.Repeat("word ", 200),
			want:    "Verifier: pass: " + strings.Repeat("word ", 119) + "word…\n" + pointer,
		},
		{
			// Character 600 lands inside a word: the partial word is dropped.
			name:    "limit inside a word",
			summary: strings.Repeat("abcdefgh ", 100),
			want:    "Verifier: pass: " + strings.Repeat("abcdefgh ", 65) + "abcdefgh…\n" + pointer,
		},
		{
			name:    "exactly at the limit",
			summary: strings.Repeat("x", 600),
			want:    "Verifier: pass: " + strings.Repeat("x", 600) + "\nCost:",
		},
		{
			name:    "one over the limit",
			summary: strings.Repeat("x", 601),
			want:    "Verifier: pass: " + strings.Repeat("x", 600) + "…\n" + pointer,
		},
		{
			name:    "short",
			summary: "all good",
			want:    "Verifier: pass: all good\nCost:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, bare, _ := fixture(t)
			ctx := context.Background()
			if _, err := o.Store.AppendEvent(ctx, "task-verdict", "", "t1", map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: c.summary}}); err != nil {
				t.Fatal(err)
			}
			res, err := Publish(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			msg := git(t, bare, "log", "-1", "--format=%B", res.SHA)
			if !strings.Contains(msg, "- [x] x exists") || strings.Contains(msg, "- [ ]") {
				t.Errorf("acceptance not checked:\n%s", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("message lacks %q:\n%s", c.want, msg)
			}
			if strings.HasSuffix(c.want, "Cost:") && strings.Contains(msg, "mill review show") {
				t.Errorf("unexpected pointer line:\n%s", msg)
			}
		})
	}
}

// extraPathBuilder builds the published tree as Publish does, then adds the file name to it:
// a wrong tree, for the path check to catch.
func extraPathBuilder(name string) func(ctx context.Context, dir, start, head, parent string) (string, []string, error) {
	return func(ctx context.Context, dir, start, head, parent string) (string, []string, error) {
		tree, conflicts, err := gitx.ApplyTree(ctx, dir, start, head, parent)
		if err != nil || len(conflicts) > 0 {
			return tree, conflicts, err
		}
		ls, err := gitx.Run(ctx, dir, "ls-tree", tree)
		if err != nil {
			return "", nil, err
		}
		blob, err := gitx.RunInput(ctx, dir, "extra\n", "hash-object", "-w", "--stdin")
		if err != nil {
			return "", nil, err
		}
		tree, err = gitx.RunInput(ctx, dir, ls+"\n100644 blob "+blob+"\t"+name+"\n", "mktree")
		return tree, nil, err
	}
}

func TestPublishRefusesCommitChangingPathsOutsideOwnChange(t *testing.T) {
	// Serial: replaces the process-wide tree builder (SetTreeBuilder).
	o, bare, _ := fixture(t)
	ctx := context.Background()
	t.Cleanup(SetTreeBuilder(extraPathBuilder("extra.txt")))

	_, err := Publish(ctx, o)
	var re *RefusedError
	if !errors.As(err, &re) || re.Rule != ruleOwnPaths || !strings.Contains(err.Error(), "extra.txt") || strings.Contains(err.Error(), "x.txt") {
		t.Fatalf("publish = %v, want a refusal naming extra.txt only", err)
	}
	if _, ok := ParentRefusal(err); ok {
		t.Error("the path refusal is reported as a dependency refusal")
	}
	if _, err := gitx.RevParse(ctx, bare, "refs/heads/review/t1"); err == nil {
		t.Error("refused publish pushed review/t1")
	}
	tk, _ := o.Store.GetTask(ctx, "t1")
	br, _ := o.Store.GetBranch(ctx, "t1")
	if tk.Status != store.TaskVerified || br.PublishedSHA != "" {
		t.Errorf("task %s, published %q; want verified and unpublished", tk.Status, br.PublishedSHA)
	}
}

func TestPublishAppliesOwnChangeOntoParentTheBranchLacks(t *testing.T) {
	t.Parallel()
	o, bare, br := fixture(t)
	ctx := context.Background()
	root := o.Paths.Root
	// The base gains other.txt after the task started; the task's branch does not have it,
	// but its recorded base (the published commit's parent) does.
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("base change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "other.txt")
	git(t, root, "commit", "-q", "-m", "base gains other.txt")
	git(t, root, "push", "-q", "origin", "main")
	tip := git(t, root, "rev-parse", "HEAD")
	br.BaseSHA = tip
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}

	res, err := Publish(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Parent != tip {
		t.Fatalf("parent = %s, want %s", res.Parent, tip)
	}
	if got := git(t, bare, "diff", "--name-only", res.Parent, res.SHA); got != "x.txt" {
		t.Errorf("published commit changes %q, want only x.txt", got)
	}
	if got := git(t, bare, "show", res.SHA+":other.txt"); got != "base change" {
		t.Errorf("other.txt in the published commit = %q, want the base's change", got)
	}
}
