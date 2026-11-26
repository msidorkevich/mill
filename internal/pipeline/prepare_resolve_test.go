package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// conflictingDeps sets up a goal where g1/a and g1/b each change README's single line and
// g1/c depends on both, runs a and b to verified, and returns the env.
func conflictingDeps(t *testing.T, resolver ...runner.Step) *env {
	t.Helper()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {goalImpl("README", "from a\n"), goalImpl("README", "from b\n"), goalImpl("c.txt", "c\n")},
		runner.RoleVerifier:    {verdict("pass"), verdict("pass"), verdict("pass")},
		runner.RoleResolver:    resolver,
	}, nil)
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	e.p.Now = func() time.Time { return clock }
	e.goal(t, nil, nil, []string{"a", "b"})
	for _, id := range []string{"g1/a", "g1/b"} {
		if out := e.run(t, id); out.Status != store.TaskVerified {
			t.Fatalf("%s: %+v\n%s", id, out, e.out)
		}
	}
	return e
}

func resolverRoles(t *testing.T, e *env, id string) []string {
	t.Helper()
	atts, err := e.p.Store.Attempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, a := range atts {
		roles = append(roles, a.Role)
	}
	return roles
}

func TestPrepareConflictResolvedByResolverContinues(t *testing.T) {
	t.Parallel()
	e := conflictingDeps(t, runner.Step{
		Output: json.RawMessage(`{"summary":"kept both lines","resolved":["README"]}`),
		Files:  map[string]string{"README": "from a and b\n"},
	})
	out := e.run(t, "g1/c")
	if out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ctx := context.Background()
	ba, _ := e.p.Store.GetBranch(ctx, "g1/a")
	bb, _ := e.p.Store.GetBranch(ctx, "g1/b")
	bc, err := e.p.Store.GetBranch(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{ba.HeadSHA, bb.HeadSHA} {
		if ok, err := gitx.IsAncestor(ctx, bc.Worktree, dep, bc.StartSHA); err != nil || !ok {
			t.Fatalf("start %s does not contain dependency head %s: %v", bc.StartSHA, dep, err)
		}
	}
	if parents, _ := gitx.Run(ctx, bc.Worktree, "rev-parse", bc.StartSHA+"^@"); len(strings.Fields(parents)) != 2 {
		t.Fatalf("start is not a merge commit: %q", parents)
	}
	if got, _ := gitx.Run(ctx, bc.Worktree, "show", bc.StartSHA+":README"); got != "from a and b" {
		t.Fatalf("merged README = %q", got)
	}
	if roles := strings.Join(resolverRoles(t, e, "g1/c"), ","); roles != "resolver,implementer,verifier" {
		t.Fatalf("attempt roles = %s", roles)
	}
	var call runner.Request
	for _, c := range e.fake.Calls() {
		if c.Role == runner.RoleResolver {
			call = c
		}
	}
	if call.Dir != bc.Worktree || call.TaskID != "g1/c" {
		t.Fatalf("resolver ran in %q for %q, want %q for g1/c", call.Dir, call.TaskID, bc.Worktree)
	}
	for _, want := range []string{"<<<<<<<", "from a", "from b", "@@ README", "mill: g1/a attempt 1", "mill: g1/b attempt 1", "(branch mill/g1/"} {
		if !strings.Contains(call.Prompt, want) {
			t.Fatalf("resolver prompt lacks %q:\n%s", want, call.Prompt)
		}
	}
	if !strings.Contains(e.out.String(), "g1/c: resolver resolved the conflict with dependency") {
		t.Fatalf("log lacks the resolution:\n%s", e.out)
	}
}

func TestPrepareConflictResolverFailsBlocksTask(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		step runner.Step
		want string
	}{
		"markers left": {
			step: runner.Step{Output: json.RawMessage(`{"summary":"could not decide which line wins","resolved":[]}`)},
			want: "conflict markers remain in README (the resolver said: could not decide which line wins)",
		},
		"run failed": {
			step: runner.Step{Error: "agent crashed"},
			want: "the resolver run failed: agent crashed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := conflictingDeps(t, tc.step)
			out := e.run(t, "g1/c")
			if out.Status != store.TaskBlocked {
				t.Fatalf("outcome = %+v\n%s", out, e.out)
			}
			for _, want := range []string{"conflicts with the other dependencies in README; resolve the conflict in the plan or by hand; the resolver failed: ", tc.want} {
				if !strings.Contains(out.Reason, want) {
					t.Fatalf("reason %q lacks %q", out.Reason, want)
				}
			}
			ctx := context.Background()
			task, _ := e.p.Store.GetTask(ctx, "g1/c")
			if task.Status != store.TaskBlocked || task.Reason != out.Reason {
				t.Fatalf("task = %s %q", task.Status, task.Reason)
			}
			if roles := strings.Join(resolverRoles(t, e, "g1/c"), ","); roles != "resolver" {
				t.Fatalf("attempt roles = %s", roles)
			}
			if gitx.BranchExists(ctx, e.root, "mill/g1/c") {
				t.Fatal("branch left behind")
			}
			wt := filepath.Join(e.p.Cfg.Worktrees(e.root, e.p.Home), "g1", "c")
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Fatalf("worktree left behind: %v", err)
			}
			if wts, _ := gitx.Run(ctx, e.root, "worktree", "list", "--porcelain"); strings.Contains(wts, "mill/g1/c") {
				t.Fatalf("worktree still registered:\n%s", wts)
			}
			if _, err := gitx.Run(ctx, e.root, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
				t.Fatal("a merge is in progress in the repository")
			}
		})
	}
}
