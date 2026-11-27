package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
)

// owndSetup builds an env under the warn owns policy and a single attempt, whose fake
// implementer writes one set of files per run, each run followed by a passing verdict.
func owndSetup(t *testing.T, runs ...map[string]string) *env {
	t.Helper()
	script := runner.Script{}
	for _, files := range runs {
		script[runner.RoleImplementer] = append(script[runner.RoleImplementer], impl(files))
		script[runner.RoleVerifier] = append(script[runner.RoleVerifier], verdict("pass"))
	}
	return setup(t, `true`, script, func(c *config.Config) { c.OwnsPolicy = "warn"; c.MaxAttempts = 1 })
}

// owndAssertExempt checks that the run of task id, which edited a.txt of the verified but
// not landed g1/a, was accepted, noted and shown to the verifier.
func owndAssertExempt(t *testing.T, e *env, id string) {
	t.Helper()
	a, err := e.p.Store.GetTask(context.Background(), "g1/a")
	if err != nil || a.Status != store.TaskVerified {
		t.Fatalf("g1/a = %+v, %v; want verified and not landed", a, err)
	}
	if out := e.run(t, id); out.Status != store.TaskVerified || out.Attempts != 1 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if strings.Contains(e.out.String(), "owned by") {
		t.Fatalf("log names a dependency as owner:\n%s", e.out)
	}
	if ow := e.ownsOwners(t, id); len(ow) != 1 || len(ow[0]) != 0 {
		t.Fatalf("owners = %+v", ow)
	}
	if files := e.ownsFiles(t, id); len(files) != 1 || strings.Join(files[0], ",") != "a.txt" {
		t.Fatalf("owns-outside files = %+v", files)
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) == 0 || !strings.Contains(ps[len(ps)-1], "- a.txt") {
		t.Fatalf("verifier prompts:\n%v", ps)
	}
	diff, err := os.ReadFile(filepath.Join(e.p.Paths.Runs, filepath.FromSlash(id), "a1", "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "+edited by a dependent") {
		t.Fatalf("diff lacks the edit to a.txt:\n%s", diff)
	}
}

func TestOwnsDirectDependencyFileIsNotAConflict(t *testing.T) {
	t.Parallel()
	e := owndSetup(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"b.txt": "b\n", "a.txt": "edited by a dependent\n"},
	)
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"b.txt"}, deps: []string{"a"}},
	)
	e.runAll(t, "g1/a")
	owndAssertExempt(t, e, "g1/b")
}

func TestOwnsTransitiveDependencyFileIsNotAConflict(t *testing.T) {
	t.Parallel()
	e := owndSetup(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"b.txt": "b\n"},
		map[string]string{"c.txt": "c\n", "a.txt": "edited by a dependent\n"},
	)
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"b.txt"}, deps: []string{"a"}},
		goalTask{id: "c", owns: []string{"c.txt"}, deps: []string{"b"}},
	)
	e.runAll(t, "g1/a", "g1/b")
	owndAssertExempt(t, e, "g1/c")
}

func TestOwnsUnrelatedSiblingFileIsRefusedAlongsideDependency(t *testing.T) {
	t.Parallel()
	e := owndSetup(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"b.txt": "b\n", "a.txt": "x\n", "s.txt": "x\n"},
	)
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"b.txt"}, deps: []string{"a"}},
		goalTask{id: "s", owns: []string{"s.txt"}},
	)
	e.runAll(t, "g1/a")
	if out := e.run(t, "g1/b"); out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	f, err := LastFindings(context.Background(), e.p.Store, "g1/b")
	if err != nil || len(f) != 1 || f[0].File != "s.txt" || !strings.Contains(f[0].Issue, "owned by task g1/s") {
		t.Fatalf("findings = %+v, %v", f, err)
	}
	if ow := e.ownsOwners(t, "g1/b"); len(ow) != 1 || len(ow[0]) != 1 || strings.Join(ow[0]["s.txt"], ",") != "g1/s" {
		t.Fatalf("owners = %+v", ow)
	}
}

// owndVerdict is a verdict step whose findings all name file.
func owndVerdict(v, file string, issues ...string) runner.Step {
	type f struct {
		File  string `json:"file"`
		Line  int    `json:"line"`
		Issue string `json:"issue"`
	}
	fs := []f{}
	for _, issue := range issues {
		fs = append(fs, f{file, 1, issue})
	}
	b, err := json.Marshal(map[string]any{"verdict": v, "summary": "review of the change", "findings": fs})
	if err != nil {
		panic(err)
	}
	return runner.Step{Output: b}
}

func TestOwnsVerifierCanRejectEditToDependencyFile(t *testing.T) {
	t.Parallel()
	script := runner.Script{
		runner.RoleImplementer: {
			impl(map[string]string{"a.txt": "a\n"}),
			impl(map[string]string{"b.txt": "b\n", "a.txt": "edited by a dependent\n"}),
		},
		runner.RoleVerifier: {
			verdict("pass"),
			owndVerdict("fail", "a.txt", "the edit to a.txt breaks a"),
		},
	}
	e := setup(t, `true`, script, func(c *config.Config) { c.OwnsPolicy = "warn"; c.MaxAttempts = 1 })
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"b.txt"}, deps: []string{"a"}},
	)
	e.runAll(t, "g1/a")
	if out := e.run(t, "g1/b"); out.Status == store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	b, err := e.p.Store.GetTask(context.Background(), "g1/b")
	if err != nil || b.Status == store.TaskVerified {
		t.Fatalf("g1/b = %+v, %v; want not verified", b, err)
	}
	f, err := LastFindings(context.Background(), e.p.Store, "g1/b")
	if err != nil || len(f) != 1 || f[0].File != "a.txt" || !strings.Contains(f[0].Issue, "breaks a") {
		t.Fatalf("findings = %+v, %v", f, err)
	}
	evs, err := e.p.Store.TaskEvents(context.Background(), "g1/b")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == "verifier-out-of-scope" {
			t.Fatalf("verifier-out-of-scope event swallowed the finding: %s", ev.Data)
		}
	}
	if files := e.ownsFiles(t, "g1/b"); len(files) != 1 || strings.Join(files[0], ",") != "a.txt" {
		t.Fatalf("owns-outside files = %+v", files)
	}
}
