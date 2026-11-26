package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/store"
)

func commitOnBase(t *testing.T, root, file string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, file), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(context.Background(), root, "base changes "+file); err != nil {
		t.Fatal(err)
	}
}

func baseChanged(t *testing.T, root string) []string {
	t.Helper()
	r := run(t, root, nil, "review", "show", "g1/a", "--json")
	var v struct {
		BaseChanged []string `json:"base_changed"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); r.code != ExitOK || err != nil {
		t.Fatalf("show --json: %v: %+v", err, r)
	}
	return v.BaseChanged
}

func TestReviewShowListsBaseChangesWithRebaseHint(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	const header = "Changed on the base since this task started:"

	commitOnBase(t, root, "unrelated.txt")
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || strings.Contains(r.stdout, header) || strings.Contains(r.stdout, "mill task rebase") {
		t.Fatalf("show with an unrelated base change: %+v", r)
	}
	r = run(t, root, nil, "review", "show", "g1/a", "--json")
	if r.code != ExitOK || strings.Contains(r.stdout, "base_changed") {
		t.Fatalf("show --json with an unrelated base change: %+v", r)
	}

	commitOnBase(t, root, "a.txt")
	block := header + "\n  a.txt\n"
	for _, args := range [][]string{{"review", "show", "g1/a"}, {"review", "show", "g1/a", "--diff"}} {
		r = run(t, root, nil, args...)
		if r.code != ExitOK || !strings.Contains(r.stdout, block+"Hint: `mill task rebase g1/a`\n") ||
			strings.Contains(r.stdout, "mill keep") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if got := baseChanged(t, root); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("base_changed = %v", got)
	}

	setStatus(t, root, "g1/a", store.TaskPublished)
	r = run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, block+"Hint: `mill keep` restacks published tasks.\n") ||
		strings.Contains(r.stdout, "mill task rebase") {
		t.Fatalf("show published: %+v", r)
	}
}

func TestReviewShowListsBaseChangesFailsWhenBaseIsGone(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	s := openStore(t, root)
	ctx := context.Background()
	br, err := s.GetBranch(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	br.Base = "gone"
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "find the base changes of task g1/a") {
		t.Fatalf("show: %+v", r)
	}
}
