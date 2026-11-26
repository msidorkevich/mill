package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// baseRepo is a temporary repository with a task branch started from main.
type baseRepo struct {
	root string
	br   store.Branch
}

func newBaseRepo(t *testing.T) *baseRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &baseRepo{root: t.TempDir()}
	r.git(t, "init", "-q", "-b", "main")
	sha := r.commit(t, "owned/a.txt", "a\n")
	r.git(t, "branch", "mill/t1")
	r.br = store.Branch{TaskID: "t1", Name: "mill/t1", Base: "main", BaseSHA: sha, StartSHA: sha, HeadSHA: sha}
	return r
}

func (r *baseRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), r.root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *baseRepo) commit(t *testing.T, file, content string) string {
	t.Helper()
	path := filepath.Join(r.root, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := gitx.CommitAll(context.Background(), r.root, "change "+file)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func (r *baseRepo) find(t *testing.T, owns ...string) (BaseChanges, error) {
	t.Helper()
	task := store.Task{Spec: taskspec.Spec{ID: "t1", Owns: owns}}
	return FindBaseChanges(context.Background(), r.root, config.Default(), task, r.br)
}

func TestFindBaseChangesListsOwnedFilesChangedOnBase(t *testing.T) {
	t.Parallel()
	r := newBaseRepo(t)
	r.commit(t, "owned/a.txt", "changed\n")
	tip := r.commit(t, "unrelated.txt", "u\n")

	got, err := r.find(t, "owned/**", "owned/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != "main" || got.From != r.br.BaseSHA || got.To != tip || !slices.Equal(got.Files, []string{"owned/a.txt"}) {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.Stat, "owned/a.txt") || strings.Contains(got.Stat, "unrelated.txt") {
		t.Fatalf("stat = %q", got.Stat)
	}
}

func TestFindBaseChangesListsFilesTheTaskChanged(t *testing.T) {
	t.Parallel()
	r := newBaseRepo(t)
	r.git(t, "checkout", "-q", "mill/t1")
	r.commit(t, "z.txt", "task\n")
	r.br.HeadSHA = r.commit(t, "b.txt", "task\n")
	r.git(t, "checkout", "-q", "main")
	r.commit(t, "z.txt", "base\n")
	r.commit(t, "b.txt", "base\n")
	r.commit(t, "unrelated.txt", "u\n")
	r.br.BaseSHA = "" // From falls back to StartSHA.

	got, err := r.find(t)
	if err != nil {
		t.Fatal(err)
	}
	if got.From != r.br.StartSHA || !slices.Equal(got.Files, []string{"b.txt", "z.txt"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFindBaseChangesPrefersRemoteTrackingBase(t *testing.T) {
	t.Parallel()
	r := newBaseRepo(t)
	remote := r.commit(t, "owned/a.txt", "remote\n")
	r.git(t, "update-ref", "refs/remotes/origin/main", remote)
	r.git(t, "reset", "-q", "--hard", r.br.BaseSHA)

	got, err := r.find(t, "owned/*")
	if err != nil {
		t.Fatal(err)
	}
	if got.To != remote || !slices.Equal(got.Files, []string{"owned/a.txt"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFindBaseChangesEmptyWhenBaseUnchanged(t *testing.T) {
	t.Parallel()
	r := newBaseRepo(t)
	got, err := r.find(t, "owned/**")
	if err != nil {
		t.Fatal(err)
	}
	if got.From != got.To || len(got.Files) != 0 || got.Stat != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestFindBaseChangesEmptyWhenNothingIntersects(t *testing.T) {
	t.Parallel()
	r := newBaseRepo(t)
	r.commit(t, "unrelated.txt", "u\n")
	got, err := r.find(t, "owned/**")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 || got.Stat != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestFindBaseChangesFailsWhenGitCannotDecide(t *testing.T) {
	t.Parallel()
	missing := strings.Repeat("1", 40)
	for name, breakBranch := range map[string]func(*store.Branch){
		"missing base commit": func(b *store.Branch) { b.BaseSHA = missing },
		"missing head commit": func(b *store.Branch) { b.HeadSHA = missing },
		"no head recorded":    func(b *store.Branch) { b.HeadSHA = "" },
		"missing base branch": func(b *store.Branch) { b.Base = "gone" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newBaseRepo(t)
			r.commit(t, "owned/a.txt", "changed\n")
			breakBranch(&r.br)
			if got, err := r.find(t, "owned/**"); err == nil {
				t.Fatalf("no error, got %+v", got)
			}
		})
	}
}
