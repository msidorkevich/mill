package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// afterImplement runs then once the implementer has returned: the task has started and has
// not been verified yet.
type afterImplement struct {
	runner.Runner
	then func() error
}

func (a afterImplement) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	res, err := a.Runner.Run(ctx, req)
	if err == nil && req.Role == runner.RoleImplementer {
		err = a.then()
	}
	return res, err
}

// ownedTaskEnv is an env with task "owner", which owns hello.txt, and then run between its
// implementer and its verifier.
func ownedTaskEnv(t *testing.T, then func(root string) error, verdicts ...runner.Step) *env {
	t.Helper()
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    verdicts,
	}, nil)
	spec := taskspec.Spec{ID: "owner", Title: "Owner", Kind: "implement", Spec: "create hello.txt",
		Acceptance: []string{"hello.txt says hello"}, Owns: []string{"hello.txt"}, Size: "S"}
	if err := e.p.Store.CreateTask(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	e.p.Runner = afterImplement{e.fake, func() error { return then(e.root) }}
	return e
}

func commitOwnedFileOnBase(root string) error {
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("from the base\n"), 0o644); err != nil {
		return err
	}
	_, err := gitx.CommitAll(context.Background(), root, "change hello.txt on the base")
	return err
}

func baseChangedEvents(t *testing.T, e *env, id string) []map[string]any {
	t.Helper()
	events, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Kind != "base-changed" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestVerifierPromptGetsBaseChangesForOwnedFile(t *testing.T) {
	t.Parallel()
	e := ownedTaskEnv(t, commitOwnedFileOnBase, verdict("pass"), verdict("pass"))
	const id = "owner"
	if out := e.run(t, id); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	br, err := e.p.Store.GetBranch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := gitx.RevParse(context.Background(), e.root, "main")
	if err != nil {
		t.Fatal(err)
	}
	// The second verification is the keeper's.
	if _, stop, err := e.p.Verify(context.Background(), id); err != nil || stop != "" {
		t.Fatalf("Verify: stop=%q err=%v", stop, err)
	}

	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 2 {
		t.Fatalf("verifier prompts = %d", len(ps))
	}
	for _, p := range ps {
		_, section, ok := strings.Cut(p, "Changed on the base since this task started")
		if !ok || !strings.Contains(section, "hello.txt") || !strings.Contains(section, "1 file changed") {
			t.Fatalf("prompt lacks the base changes:\n%s", p)
		}
	}
	events := baseChangedEvents(t, e, id)
	if len(events) != 2 {
		t.Fatalf("base-changed events = %v", events)
	}
	for i, ev := range events {
		files, _ := json.Marshal(ev["files"])
		if ev["attempt"] != float64(i+1) || ev["base"] != "main" || ev["from"] != br.BaseSHA || ev["to"] != tip ||
			!slices.Equal(files, []byte(`["hello.txt"]`)) {
			t.Fatalf("event %d = %v", i, ev)
		}
	}
	// Nothing is rebased: the task branch stays where it was.
	head, err := gitx.RevParse(context.Background(), br.Worktree, "HEAD")
	if err != nil || head != br.HeadSHA {
		t.Fatalf("head = %s, want %s (%v)", head, br.HeadSHA, err)
	}
}
