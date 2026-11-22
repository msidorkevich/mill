package syncer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
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

func writeCommit(t *testing.T, dir, name, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// fixture is a repo with a bare remote and one verified task per id, each on its own branch
// mill/t/<id> off main with one commit adding <id>.txt.
func fixture(t *testing.T, ids ...string) (Options, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	base := writeCommit(t, root, "README", "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-q", "origin", "main")

	paths := project.PathsAt(root)
	if err := os.MkdirAll(paths.MillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.DB, store.Options{Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, id := range ids {
		wt := filepath.Join(t.TempDir(), id)
		name := "mill/t/" + id
		git(t, root, "worktree", "add", "-q", "-b", name, wt, base)
		head := writeCommit(t, wt, id+".txt", "work on "+id)
		spec := taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Size: "S"}
		if err := s.CreateTask(ctx, spec); err != nil {
			t.Fatal(err)
		}
		br := store.Branch{TaskID: id, Name: name, Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: head, GateSHA: head, Worktree: wt}
		if err := s.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, id, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.CheckCmd = "true"
	return Options{Store: s, Cfg: cfg, Paths: paths}, bare
}

func TestSyncMarksVerifiedBranchMergedDirectlyAsLanded(t *testing.T) {
	o, bare := fixture(t, "merged", "pending")
	ctx := context.Background()

	// Someone merges the task branch itself (not a published review) into the remote main.
	forge := filepath.Join(t.TempDir(), "forge")
	git(t, t.TempDir(), "clone", "-q", bare, forge)
	git(t, forge, "checkout", "-q", "-B", "main", "origin/main")
	git(t, o.Paths.Root, "push", "-q", bare, "mill/t/merged:refs/heads/feature")
	git(t, forge, "fetch", "-q", "origin")
	git(t, forge, "merge", "-q", "--no-ff", "-m", "Merge feature", "origin/feature")
	merge := git(t, forge, "rev-parse", "HEAD")
	git(t, forge, "push", "-q", "origin", "main")

	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 1 || res.Landed[0] != (Landed{TaskID: "merged", Method: MethodMerged, Commit: merge}) {
		t.Fatalf("landed = %+v", res.Landed)
	}
	if res.BaseSHA != merge {
		t.Errorf("base sha = %s, want %s", res.BaseSHA, merge)
	}
	if tk, _ := o.Store.GetTask(ctx, "merged"); tk.Status != store.TaskLanded {
		t.Errorf("merged status = %s", tk.Status)
	}
	if tk, _ := o.Store.GetTask(ctx, "pending"); tk.Status != store.TaskVerified {
		t.Errorf("pending status = %s", tk.Status)
	}
	evs, err := o.Store.TaskEvents(ctx, "merged")
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	var d map[string]string
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatal(err)
	}
	br, _ := o.Store.GetBranch(ctx, "merged")
	if last.Kind != "task-landed" || d["method"] != "merged" || d["commit"] != merge || d["head_sha"] != br.HeadSHA {
		t.Errorf("last event = %s %v", last.Kind, d)
	}

	// A further sync finds nothing new.
	res, err = Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 0 {
		t.Errorf("second sync landed = %+v", res.Landed)
	}
}

func TestSyncDoesNotLandVerifiedTaskWithoutOwnCommits(t *testing.T) {
	o, _ := fixture(t, "empty")
	ctx := context.Background()
	br, _ := o.Store.GetBranch(ctx, "empty")
	br.HeadSHA, br.GateSHA = br.BaseSHA, br.BaseSHA
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Landed) != 0 {
		t.Errorf("landed = %+v", res.Landed)
	}
	if tk, _ := o.Store.GetTask(ctx, "empty"); tk.Status != store.TaskVerified {
		t.Errorf("status = %s", tk.Status)
	}
}
