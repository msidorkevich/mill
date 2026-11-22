package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

const validPlan = `{
  "goal": "Add greeting",
  "assumptions": ["Go project"],
  "tasks": [
    {"id": "b", "title": "Second", "kind": "implement", "spec": "do b", "acceptance": ["b works"],
     "checks": ["true"], "owns": ["b.go"], "depends_on": ["a"], "size": "S"},
    {"id": "a", "title": "First", "kind": "implement", "spec": "do a", "acceptance": ["a works"],
     "checks": ["echo checking-a"], "owns": ["a.go"], "depends_on": [], "size": "S"}
  ]
}`

const overlapPlan = `{
  "goal": "Overlap",
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["same.go"], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["same.go"], "size": "S"}
  ]
}`

const invalidPlan = `{"goal": "", "tasks": [
  {"id": "a", "title": "A", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": [], "depends_on": ["zzz"], "size": "S"}
]}`

func planRepo(t *testing.T) string {
	t.Helper()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	return root
}

func writePlan(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanFileCreatesDraftGoal(t *testing.T) {
	root := planRepo(t)
	r := run(t, root, nil, "plan", "--file", writePlan(t, overlapPlan))
	if r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	for _, want := range []string{"Created goal g1 (draft): Overlap", "2 tasks", "warning:", "mill plan show g1", "mill plan approve g1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	l := run(t, root, nil, "goal", "list")
	if !strings.Contains(l.stdout, "g1") || !strings.Contains(l.stdout, "draft") || !strings.Contains(l.stdout, "0/0") {
		t.Errorf("goal list:\n%s", l.stdout)
	}
}

func TestPlanFileInvalidReportsEveryProblemAndCreatesNoGoal(t *testing.T) {
	root := planRepo(t)
	r := run(t, root, nil, "plan", "--file", writePlan(t, invalidPlan))
	if r.code != ExitError {
		t.Fatalf("code = %d: %+v", r.code, r)
	}
	for _, want := range []string{"goal must not be empty", "zzz", "at least one check"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, r.stderr)
		}
	}
	if l := run(t, root, nil, "goal", "list"); !strings.Contains(l.stdout, "no goals") {
		t.Errorf("a goal was created:\n%s", l.stdout)
	}
}

// plannerRepo is an initialized repo whose fake runner answers the planner with outputs.
func plannerRepo(t *testing.T, outputs ...string) string {
	t.Helper()
	root := planRepo(t)
	var steps []string
	for _, o := range outputs {
		steps = append(steps, `{"output": `+o+`, "usage": [{"input_tokens": 1000, "output_tokens": 234, "cost_usd": 0.5}]}`)
	}
	script := `{"planner": [` + strings.Join(steps, ",") + `]}`
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "check_cmd = \"true\"\n[runner]\nkind = \"fake\"\nfake_script = \"script.json\"\n"
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPlanGoalTextCreatesDraftGoalWithPlanningCost(t *testing.T) {
	root := plannerRepo(t, invalidPlan, validPlan)
	r := run(t, root, nil, "plan", "add a greeting")
	if r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	for _, want := range []string{"Created goal g1 (draft): Add greeting", "2 tasks", "mill plan approve g1",
		"Planning cost: 2,468 tokens (billable), $1.00 in 2 agent call(s)"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if s := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(s.stdout, "First") || !strings.Contains(s.stdout, "draft") {
		t.Errorf("plan show:\n%s", s.stdout)
	}
	plans, err := filepath.Glob(filepath.Join(project.PathsAt(root).Runs, "plans", "*", "call2"))
	if err != nil || len(plans) != 1 {
		t.Errorf("planner logs not under .mill/runs/plans/<timestamp>/: %v %v", plans, err)
	}
}

func TestPlanGoalTextInvalidTwiceSavesPlanAndCreatesNoGoal(t *testing.T) {
	root := plannerRepo(t, invalidPlan, invalidPlan)
	r := run(t, root, nil, "plan", "add a greeting")
	if r.code != ExitError {
		t.Fatalf("code = %d: %+v", r.code, r)
	}
	for _, want := range []string{"goal must not be empty", "zzz", "No goal was created", "mill plan --file "} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, r.stderr)
		}
	}
	i := strings.Index(r.stderr, "mill plan --file ")
	path := strings.TrimSuffix(strings.Fields(r.stderr[i+len("mill plan --file "):])[0], "`.")
	if !strings.HasPrefix(path, filepath.Join(project.PathsAt(root).Runs, "plans")) || filepath.Base(path) != "plan.json" {
		t.Errorf("plan path %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("saved plan: %v", err)
	}
	var got, want any
	if json.Unmarshal(data, &got) != nil || json.Unmarshal([]byte(invalidPlan), &want) != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("saved plan = %s", data)
	}
	if l := run(t, root, nil, "goal", "list"); !strings.Contains(l.stdout, "no goals") {
		t.Errorf("a goal was created:\n%s", l.stdout)
	}
	if r := run(t, root, nil, "plan"); r.code != ExitUsage {
		t.Errorf("plan alone: %+v", r)
	}
}

func TestPlanShowDependencyOrderAndUntrustedChecks(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	r := run(t, root, nil, "plan", "show", "g1")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	for _, want := range []string{"g1: Add greeting", "draft", "Go project", "untrusted", "echo checking-a", "a.go", "a works", "depends_on  a"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Index(r.stdout, "1. a:") < 0 || strings.Index(r.stdout, "2. b:") < 0 {
		t.Errorf("tasks not in dependency order:\n%s", r.stdout)
	}

	j := run(t, root, nil, "plan", "show", "g1", "--json")
	var doc struct {
		Schema string `json:"schema"`
		Goal   struct{ ID, Status string }
		Order  []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(j.stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, j.stdout)
	}
	if doc.Schema != "mill/plan@1" || doc.Goal.ID != "g1" || len(doc.Order) != 2 || doc.Order[0].ID != "a" {
		t.Errorf("doc: %+v", doc)
	}
}

func TestPlanEditFileReplacesDraftPlan(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	r := run(t, root, nil, "plan", "edit", "g1", "--file", writePlan(t, overlapPlan))
	if r.code != ExitOK || !strings.Contains(r.stdout, "warning:") {
		t.Fatalf("edit: %+v", r)
	}
	if s := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(s.stdout, "same.go") {
		t.Errorf("plan not replaced:\n%s", s.stdout)
	}
}

func TestPlanEditFileInvalidChangesNothing(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	r := run(t, root, nil, "plan", "edit", "g1", "--file", writePlan(t, invalidPlan))
	if r.code != ExitError || !strings.Contains(r.stderr, "goal must not be empty") {
		t.Fatalf("edit: %+v", r)
	}
	if s := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(s.stdout, "Add greeting") {
		t.Errorf("plan changed:\n%s", s.stdout)
	}
}

func TestPlanEditApprovedGoalFails(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	run(t, root, nil, "plan", "approve", "g1")
	r := run(t, root, nil, "plan", "edit", "g1", "--file", writePlan(t, overlapPlan))
	if r.code != ExitError || !strings.Contains(r.stderr, "approved") {
		t.Errorf("edit approved: %+v", r)
	}
}

func editorScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanEditRunsEditorAndAppliesValidResult(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	t.Setenv("EDITOR", editorScript(t, `sed 's/First/Edited task/' "$1" > "$1.new" && mv "$1.new" "$1"`))
	r := run(t, root, nil, "plan", "edit", "g1")
	if r.code != ExitOK {
		t.Fatalf("edit: %+v", r)
	}
	if s := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(s.stdout, "Edited task") {
		t.Errorf("edit not applied:\n%s", s.stdout)
	}
}

func TestPlanEditInvalidEditorResultKeepsFile(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	t.Setenv("EDITOR", editorScript(t, `echo '{"goal": ""}' > "$1"`))
	r := run(t, root, nil, "plan", "edit", "g1")
	if r.code != ExitError || !strings.Contains(r.stderr, "goal must not be empty") {
		t.Fatalf("edit: %+v", r)
	}
	i := strings.Index(r.stderr, "kept in ")
	if i < 0 {
		t.Fatalf("no kept path:\n%s", r.stderr)
	}
	path := strings.Fields(r.stderr[i+len("kept in "):])[0]
	defer os.Remove(path)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("kept file missing: %v", err)
	}
	if !strings.Contains(r.stderr, "mill plan edit g1 --file "+path) {
		t.Errorf("no retry hint:\n%s", r.stderr)
	}
	if s := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(s.stdout, "Add greeting") {
		t.Errorf("plan changed:\n%s", s.stdout)
	}
}

func TestPlanApproveCreatesTasksAndRefusesNonDraft(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	r := run(t, root, nil, "plan", "approve", "g1")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Approved g1: 2 tasks created") || !strings.Contains(r.stdout, "mill run g1") {
		t.Fatalf("approve: %+v", r)
	}
	g := run(t, root, nil, "goal", "show", "g1")
	for _, want := range []string{"DEPENDS ON", "ATTEMPTS", "approved", "pending"} {
		if !strings.Contains(g.stdout, want) {
			t.Errorf("goal show missing %q:\n%s", want, g.stdout)
		}
	}
	if again := run(t, root, nil, "plan", "approve", "g1"); again.code != ExitError || !strings.Contains(again.stderr, "not draft") {
		t.Errorf("second approve: %+v", again)
	}
	if s := run(t, root, nil, "plan", "show", "g1"); strings.Contains(s.stdout, "untrusted") || !strings.Contains(s.stdout, "pending") {
		t.Errorf("approved show:\n%s", s.stdout)
	}
}

func TestGoalListAndShowJSON(t *testing.T) {
	root := planRepo(t)
	if r := run(t, root, nil, "goal", "list"); r.code != ExitOK || !strings.Contains(r.stdout, "no goals") {
		t.Errorf("empty list: %+v", r)
	}
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	run(t, root, nil, "plan", "approve", "g1")

	l := run(t, root, nil, "goal", "list", "--json")
	var ld struct {
		Schema string
		Goals  []struct {
			ID            string
			TasksTotal    int `json:"tasks_total"`
			TasksVerified int `json:"tasks_verified"`
		}
	}
	if err := json.Unmarshal([]byte(l.stdout), &ld); err != nil {
		t.Fatal(err)
	}
	if ld.Schema != "mill/goal-list@1" || len(ld.Goals) != 1 || ld.Goals[0].TasksTotal != 2 {
		t.Errorf("list doc: %+v", ld)
	}
	if h := run(t, root, nil, "goal", "list"); !strings.Contains(h.stdout, "TOKENS") || !strings.Contains(h.stdout, "0/2") {
		t.Errorf("list:\n%s", h.stdout)
	}

	s := run(t, root, nil, "goal", "show", "g1", "--json")
	var sd struct {
		Schema string
		Tasks  []struct {
			LocalID   string   `json:"local_id"`
			DependsOn []string `json:"depends_on"`
		}
	}
	if err := json.Unmarshal([]byte(s.stdout), &sd); err != nil {
		t.Fatal(err)
	}
	if sd.Schema != "mill/goal@1" || len(sd.Tasks) != 2 || sd.Tasks[0].LocalID != "b" || sd.Tasks[0].DependsOn[0] != "a" {
		t.Errorf("goal doc: %+v", sd)
	}
	if r := run(t, root, nil, "goal", "show", "g9"); r.code != ExitError {
		t.Errorf("unknown goal: %+v", r)
	}
}

func TestGoalListCountsPublishedAndLandedTasksAsDone(t *testing.T) {
	root := planRepo(t)
	run(t, root, nil, "plan", "--file", writePlan(t, validPlan))
	run(t, root, nil, "plan", "approve", "g1")
	setStatus(t, root, "g1/a", store.TaskLanded)
	setStatus(t, root, "g1/b", store.TaskPublished)
	if h := run(t, root, nil, "goal", "list"); h.code != ExitOK || !strings.Contains(h.stdout, "2/2") {
		t.Errorf("list:\n%+v", h)
	}
}
