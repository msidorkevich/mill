package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// ownsGoal approves goal g1 with task a owning a.txt and task b owning b.txt.
func (e *env) ownsGoal(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var tasks []taskspec.Spec
	for _, id := range []string{"a", "b"} {
		tasks = append(tasks, taskspec.Spec{ID: id, Title: "Task " + id, Kind: "implement", Spec: "create " + id + ".txt",
			Acceptance: []string{id + ".txt exists"}, Checks: []string{}, Owns: []string{id + ".txt"}, Size: "S"})
	}
	g, err := e.p.Store.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.ApproveGoal(ctx, g.ID, plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
}

// ownsOwners returns the owners of every owns-outside event of task id.
func (e *env) ownsOwners(t *testing.T, id string) []map[string][]string {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string][]string
	for _, ev := range evs {
		if ev.Kind != "owns-outside" {
			continue
		}
		var d struct {
			Files  []string            `json:"files"`
			Owners map[string][]string `json:"owners"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d.Owners)
	}
	return out
}

func gateMarker(t *testing.T) (string, string) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	return marker, `touch ` + marker
}

func TestOwnsWarnSiblingOwnedFileFailsAttemptWithoutGate(t *testing.T) {
	t.Parallel()
	marker, check := gateMarker(t)
	e := setup(t, check, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "b.txt": "x"})},
	}, func(c *config.Config) { c.OwnsPolicy = "warn"; c.MaxAttempts = 1 })
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("gate ran despite a sibling-owned file")
	}
	if len(prompts(e.fake, runner.RoleVerifier)) != 0 {
		t.Fatal("verifier ran despite a sibling-owned file")
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "g1/a")
	if len(f) != 1 || f[0].Source != "mill" || f[0].File != "b.txt" ||
		!strings.Contains(f[0].Issue, "owned by task g1/b") || !strings.Contains(f[0].Issue, "git restore --source ") || !strings.Contains(f[0].Issue, " -- b.txt") {
		t.Fatalf("findings = %+v", f)
	}
	if !strings.Contains(e.out.String(), "g1/a a1 owns      1 file(s) owned by g1/b") {
		t.Fatalf("log lacks owner line:\n%s", e.out)
	}
	ow := e.ownsOwners(t, "g1/a")
	if len(ow) != 1 || len(ow[0]) != 1 || len(ow[0]["b.txt"]) != 1 || ow[0]["b.txt"][0] != "g1/b" {
		t.Fatalf("owners = %+v", ow)
	}
}

func TestOwnsWarnUnownedFileReachesGateAndVerifier(t *testing.T) {
	t.Parallel()
	marker, check := gateMarker(t)
	e := setup(t, check, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "stray.txt": "x"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.OwnsPolicy = "warn" })
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("gate did not run")
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 1 || !strings.Contains(ps[0], "- stray.txt") {
		t.Fatalf("verifier prompt:\n%v", ps)
	}
	if ow := e.ownsOwners(t, "g1/a"); len(ow) != 1 || len(ow[0]) != 0 {
		t.Fatalf("owners = %+v", ow)
	}
}

func TestOwnsLandedSiblingFileCountsAsUnowned(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "b.txt": "x"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.OwnsPolicy = "warn" })
	e.ownsGoal(t)
	if err := e.p.Store.SetTaskStatus(context.Background(), "g1/b", store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ps := prompts(e.fake, runner.RoleVerifier)
	if len(ps) != 1 || !strings.Contains(ps[0], "- b.txt") {
		t.Fatalf("verifier prompt:\n%v", ps)
	}
	if ow := e.ownsOwners(t, "g1/a"); len(ow) != 1 || len(ow[0]) != 0 {
		t.Fatalf("owners = %+v", ow)
	}
}

func TestOwnsEnforceNamesSiblingOwnerAndFlagsOtherFiles(t *testing.T) {
	t.Parallel()
	marker, check := gateMarker(t)
	e := setup(t, check, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "b.txt": "x", "stray.txt": "x"})},
	}, func(c *config.Config) { c.OwnsPolicy = "enforce"; c.MaxAttempts = 1 })
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("gate ran despite enforce findings")
	}
	f, _ := LastFindings(context.Background(), e.p.Store, "g1/a")
	byFile := map[string]string{}
	for _, x := range f {
		byFile[x.File] = x.Issue
	}
	if len(f) != 2 || !strings.Contains(byFile["b.txt"], "owned by task g1/b") ||
		!strings.Contains(byFile["stray.txt"], "outside the task's owns") {
		t.Fatalf("findings = %+v", f)
	}
	if ow := e.ownsOwners(t, "g1/a"); len(ow) != 1 || len(ow[0]) != 1 || ow[0]["b.txt"][0] != "g1/b" {
		t.Fatalf("owners = %+v", ow)
	}
}

func TestOwnsOffIgnoresSiblingOwnedFiles(t *testing.T) {
	t.Parallel()
	marker, check := gateMarker(t)
	e := setup(t, check, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "b.txt": "x"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.OwnsPolicy = "off" })
	e.ownsGoal(t)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("gate did not run")
	}
	if e.ownsEvents(t, "g1/a") != 0 || strings.Contains(e.out.String(), "owned by") {
		t.Fatalf("off policy recorded owns:\n%s", e.out)
	}
}

// goalTask is one task of a goal built by ownsDepsGoal: its local id, owns and local
// dependencies.
type goalTask struct {
	id   string
	owns []string
	deps []string
}

// ownsDepsGoal approves goal g1 with the given tasks, in order.
func (e *env) ownsDepsGoal(t *testing.T, gts ...goalTask) {
	t.Helper()
	ctx := context.Background()
	var tasks []taskspec.Spec
	for _, gt := range gts {
		deps := gt.deps
		if deps == nil {
			deps = []string{}
		}
		tasks = append(tasks, taskspec.Spec{ID: gt.id, Title: "Task " + gt.id, Kind: "implement", Spec: "edit files",
			Acceptance: []string{"files edited"}, Checks: []string{}, Owns: gt.owns, Size: "S", DependsOn: deps})
	}
	g, err := e.p.Store.CreateGoal(ctx, "goal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.ApproveGoal(ctx, g.ID, plan.Plan{Goal: "goal", Assumptions: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
}

// ownsFiles returns the files of every owns-outside event of task id.
func (e *env) ownsFiles(t *testing.T, id string) [][]string {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, ev := range evs {
		if ev.Kind != "owns-outside" {
			continue
		}
		var d struct {
			Files []string `json:"files"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d.Files)
	}
	return out
}

func TestOwnsDependentTaskFilesAreNotConflictingOwners(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "README.md": "x", "d.txt": "x"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, func(c *config.Config) { c.OwnsPolicy = "warn" })
	// c depends on a directly and owns README.md; d depends on a through c.
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "c", owns: []string{"README.md"}, deps: []string{"a"}},
		goalTask{id: "d", owns: []string{"d.txt"}, deps: []string{"c"}},
	)
	if out := e.run(t, "g1/a"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if strings.Contains(e.out.String(), "owned by") {
		t.Fatalf("log names a dependent as owner:\n%s", e.out)
	}
	if ow := e.ownsOwners(t, "g1/a"); len(ow) != 1 || len(ow[0]) != 0 {
		t.Fatalf("owners = %+v", ow)
	}
	files := e.ownsFiles(t, "g1/a")
	if len(files) != 1 || strings.Join(files[0], ",") != "README.md,d.txt" {
		t.Fatalf("owns-outside files = %+v", files)
	}
}

func TestOwnsNonDependentSiblingStillOwnsAlongsideDependent(t *testing.T) {
	t.Parallel()
	e := setup(t, `true`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"a.txt": "a", "README.md": "x"})},
	}, func(c *config.Config) { c.OwnsPolicy = "warn"; c.MaxAttempts = 1 })
	e.ownsDepsGoal(t,
		goalTask{id: "a", owns: []string{"a.txt"}},
		goalTask{id: "b", owns: []string{"README.md"}},
		goalTask{id: "c", owns: []string{"README.md"}, deps: []string{"a"}},
	)
	if out := e.run(t, "g1/a"); out.Status != store.TaskBlocked {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	ow := e.ownsOwners(t, "g1/a")
	if len(ow) != 1 || len(ow[0]) != 1 || strings.Join(ow[0]["README.md"], ",") != "g1/b" {
		t.Fatalf("owners = %+v", ow)
	}
	if !strings.Contains(e.out.String(), "g1/a a1 owns      1 file(s) owned by g1/b\n") {
		t.Fatalf("log lacks owner line:\n%s", e.out)
	}
}
