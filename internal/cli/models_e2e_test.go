package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// These tests follow a goal from plan to stats through cli.Run only: the models the plan
// shows per complexity, the models each role ran on (with escalation after a failed
// verification), and the model statistics the run leaves behind.

// modelsE2EPlan has an S/low task, an M/high task and an S/medium task overriding to opus.
const modelsE2EPlan = `{
  "goal": "Models end to end",
  "tasks": [
    {"id": "low", "title": "Low", "kind": "implement", "spec": "write low.txt", "acceptance": ["low.txt exists"],
     "checks": ["test -f low.txt"], "owns": ["low.txt"], "size": "S", "complexity": "low", "complexity_reason": "one file"},
    {"id": "high", "title": "High", "kind": "implement", "spec": "write high.txt", "acceptance": ["high.txt exists"],
     "checks": ["test -f high.txt"], "owns": ["high.txt"], "size": "M", "complexity": "high", "complexity_reason": "tricky"},
    {"id": "pinned", "title": "Pinned", "kind": "implement", "spec": "write pinned.txt", "acceptance": ["pinned.txt exists"],
     "checks": ["test -f pinned.txt"], "owns": ["pinned.txt"], "size": "S", "complexity": "medium", "model": "opus"}
  ]
}`

// modelsE2EImpl is an implementer step writing file.
func modelsE2EImpl(file, body string) string {
	return `{"output": {"summary": "s", "files_changed": ["` + file + `"], "notes": ""}, "files": {"` + file + `": "` + body + `\n"}}`
}

// modelsE2EVerdict is a verifier step with the given verdict and, for "fail", one finding.
func modelsE2EVerdict(verdict string) string {
	findings := `[]`
	if verdict == "fail" {
		findings = `[{"file": "low.txt", "line": 1, "issue": "wrong content"}]`
	}
	return `{"output": {"verdict": "` + verdict + `", "summary": "review", "findings": ` + findings + `}}`
}

// modelsE2EScript fails the low task's first verification; every other task passes at once.
func modelsE2EScript() string {
	return `{"tasks": {
  "g1/low": {"implementer": [` + modelsE2EImpl("low.txt", "lo") + `, ` + modelsE2EImpl("low.txt", "low") + `],
             "verifier": [` + modelsE2EVerdict("fail") + `, ` + modelsE2EVerdict("pass") + `]},
  "g1/high": {"implementer": [` + modelsE2EImpl("high.txt", "high") + `], "verifier": [` + modelsE2EVerdict("pass") + `]},
  "g1/pinned": {"implementer": [` + modelsE2EImpl("pinned.txt", "pinned") + `], "verifier": [` + modelsE2EVerdict("pass") + `]}
}}`
}

// modelsE2ERepo makes an initialized repo on main with the fake script and cfgExtra appended
// to a config that runs the fake runner.
func modelsE2ERepo(t *testing.T, script, cfgExtra string) string {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "true"
max_attempts = 3
max_parallel = 2
worktree_root = "` + t.TempDir() + `"
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
` + cfgExtra
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	return root
}

// modelsE2EPlanShow returns the tasks of `plan show <id> --json`, keyed by task id.
func modelsE2EPlanShow(t *testing.T, root, id string) map[string]map[string]any {
	t.Helper()
	r := run(t, root, nil, "plan", "show", id, "--json")
	if r.code != ExitOK {
		t.Fatalf("plan show --json: %+v", r)
	}
	var doc struct {
		Order []map[string]any `json:"order"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("plan show --json: %v\n%s", err, r.stdout)
	}
	out := map[string]map[string]any{}
	for _, tk := range doc.Order {
		id, _ := tk["id"].(string)
		out[id] = tk
	}
	return out
}

// modelsE2ETaskShow is the part of `task show --json` these tests read.
type modelsE2ETaskShow struct {
	Task struct {
		Status     string `json:"status"`
		Complexity string `json:"complexity"`
	} `json:"task"`
	Attempts []store.Attempt `json:"attempts"`
}

func modelsE2EShowTask(t *testing.T, root, id string) modelsE2ETaskShow {
	t.Helper()
	r := run(t, root, nil, "task", "show", id, "--json")
	if r.code != ExitOK {
		t.Fatalf("task show %s --json: %+v", id, r)
	}
	var v modelsE2ETaskShow
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("task show %s --json: %v\n%s", id, err, r.stdout)
	}
	return v
}

// modelsE2EAttempt returns the attempt of role and number n, failing the test when absent.
func modelsE2EAttempt(t *testing.T, v modelsE2ETaskShow, id, role string, n int) store.Attempt {
	t.Helper()
	for _, a := range v.Attempts {
		if a.Role == role && a.N == n {
			return a
		}
	}
	t.Fatalf("%s: no %s attempt %d in %+v", id, role, n, v.Attempts)
	return store.Attempt{}
}

// modelsE2EWantModel checks the model and reason recorded for one attempt.
func modelsE2EWantModel(t *testing.T, v modelsE2ETaskShow, id, role string, n int, model, reason string) {
	t.Helper()
	a := modelsE2EAttempt(t, v, id, role, n)
	if a.Model != model || a.ModelReason != reason {
		t.Errorf("%s %s attempt %d ran on %q (%q), want %q (%q)", id, role, n, a.Model, a.ModelReason, model, reason)
	}
}

func TestModelsEndToEndPlanRunTaskShowAndStats(t *testing.T) {
	t.Parallel()
	root := modelsE2ERepo(t, modelsE2EScript(), "")
	if r := run(t, root, nil, "plan", "--file", writePlan(t, modelsE2EPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}

	// The plan shows the model each task maps to and why.
	plan := modelsE2EPlanShow(t, root, "g1")
	for id, want := range map[string][2]string{
		"low":    {"haiku", "mapping S/low"},
		"high":   {"opus", "mapping M/high"},
		"pinned": {"opus", "override"},
	} {
		tk, ok := plan[id]
		if !ok {
			t.Fatalf("plan show lacks %s: %v", id, plan)
		}
		if tk["model"] != want[0] || tk["model_why"] != want[1] {
			t.Errorf("plan show %s: model %v (%v), want %s (%s)", id, tk["model"], tk["model_why"], want[0], want[1])
		}
	}

	if r := run(t, root, nil, "plan", "approve", "g1"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	if r := run(t, root, nil, "run", "g1"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}

	// The low task failed its first verification and escalated one tier for attempt 2.
	low := modelsE2EShowTask(t, root, "g1/low")
	if low.Task.Status != store.TaskVerified {
		t.Errorf("g1/low status = %q, want %q", low.Task.Status, store.TaskVerified)
	}
	modelsE2EWantModel(t, low, "g1/low", store.RoleImplementer, 1, "haiku", "mapping S/low")
	modelsE2EWantModel(t, low, "g1/low", "verifier", 1, "sonnet", "verifier floor sonnet")
	modelsE2EWantModel(t, low, "g1/low", store.RoleImplementer, 2, "sonnet", "escalated after attempt 1 failed")
	modelsE2EWantModel(t, low, "g1/low", "verifier", 2, "sonnet", "implementer tier")

	high := modelsE2EShowTask(t, root, "g1/high")
	modelsE2EWantModel(t, high, "g1/high", store.RoleImplementer, 1, "opus", "mapping M/high")
	modelsE2EWantModel(t, high, "g1/high", "verifier", 1, "opus", "implementer tier")

	pinned := modelsE2EShowTask(t, root, "g1/pinned")
	modelsE2EWantModel(t, pinned, "g1/pinned", store.RoleImplementer, 1, "opus", "override")
	modelsE2EWantModel(t, pinned, "g1/pinned", "verifier", 1, "opus", "implementer tier")
	for _, v := range []struct {
		id string
		s  modelsE2ETaskShow
	}{{"g1/high", high}, {"g1/pinned", pinned}} {
		if v.s.Task.Status != store.TaskVerified || len(v.s.Attempts) != 2 {
			t.Errorf("%s: status %q with %d attempts, want verified after 2", v.id, v.s.Task.Status, len(v.s.Attempts))
		}
	}

	// Stats group tasks by size, complexity and first-attempt model.
	r := run(t, root, nil, "stats", "models", "--json")
	if r.code != ExitOK {
		t.Fatalf("stats models --json: %+v", r)
	}
	var doc struct {
		Models []store.ModelStat `json:"models"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stats models --json: %v\n%s", err, r.stdout)
	}
	want := []store.ModelStat{
		{Size: "S", Complexity: "low", Model: "haiku", Tasks: 1, FirstAttemptPass: 0, AvgAttempts: 2},
		{Size: "S", Complexity: "medium", Model: "opus", Tasks: 1, FirstAttemptPass: 1, AvgAttempts: 1},
		{Size: "M", Complexity: "high", Model: "opus", Tasks: 1, FirstAttemptPass: 1, AvgAttempts: 1},
	}
	if len(doc.Models) != len(want) {
		t.Fatalf("stats models = %+v, want %+v", doc.Models, want)
	}
	for i := range want {
		if doc.Models[i] != want[i] {
			t.Errorf("stats models row %d = %+v, want %+v", i, doc.Models[i], want[i])
		}
	}
}

// modelsE2ELegacyPlan is a plan stored before complexity existed: no task has the field.
const modelsE2ELegacyPlan = `{"goal": "Legacy", "assumptions": [], "tasks": [{"id": "old", "title": "Old", "kind": "implement",
  "spec": "write old.txt", "acceptance": ["old.txt exists"], "checks": ["true"], "owns": ["old.txt"], "depends_on": [],
  "size": "S"}]}`

// modelsE2EStoreLegacyGoal stores modelsE2ELegacyPlan as goal g1 exactly as written, without
// the defaults `plan --file` fills in. With approve set it also approves the goal from the
// plan decoded as is, so the stored plan and the task row keep an empty complexity.
func modelsE2EStoreLegacyGoal(t *testing.T, root string, approve bool) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.CreateGoal(ctx, "Legacy", json.RawMessage(modelsE2ELegacyPlan))
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "g1" || strings.Contains(string(g.Plan), "complexity") {
		t.Fatalf("stored goal %s has plan %s, want g1 without complexity", g.ID, g.Plan)
	}
	if !approve {
		return
	}
	var p plan.Plan
	if err := json.Unmarshal([]byte(modelsE2ELegacyPlan), &p); err != nil {
		t.Fatal(err)
	}
	if p.Tasks[0].Complexity != "" {
		t.Fatalf("legacy plan decoded with complexity %q", p.Tasks[0].Complexity)
	}
	if err := s.ApproveGoal(ctx, "g1", p); err != nil {
		t.Fatal(err)
	}
}

// modelsE2EWantLegacyPlanShow checks plan show reads the legacy task as medium on the sizes
// fallback model.
func modelsE2EWantLegacyPlanShow(t *testing.T, root string) {
	t.Helper()
	tk, ok := modelsE2EPlanShow(t, root, "g1")["old"]
	if !ok {
		t.Fatal("plan show lacks old")
	}
	if tk["complexity"] != "medium" || tk["model"] != "sonnet" || tk["model_why"] != "mapping S/medium (sizes fallback)" {
		t.Errorf("plan show g1/old = %v, want medium on sonnet (mapping S/medium (sizes fallback))", tk)
	}
}

func TestModelsEndToEndLegacyPlanUsesMediumAndSizesFallback(t *testing.T) {
	t.Parallel()
	cfg := "[sizes.S]\nmodel = \"sonnet\"\n"

	// A draft goal whose stored plan has no complexity.
	root := modelsE2ERepo(t, `{}`, cfg)
	modelsE2EStoreLegacyGoal(t, root, false)
	modelsE2EWantLegacyPlanShow(t, root)

	// An approved goal whose stored plan and task row both lack complexity.
	root = modelsE2ERepo(t, `{}`, cfg)
	modelsE2EStoreLegacyGoal(t, root, true)
	modelsE2EWantLegacyPlanShow(t, root)
	if v := modelsE2EShowTask(t, root, "g1/old"); v.Task.Complexity != "medium" {
		t.Errorf("task show g1/old complexity = %q, want medium", v.Task.Complexity)
	}
}
