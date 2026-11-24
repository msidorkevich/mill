package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

const editedSpec = `{"id": "a", "title": "A better", "kind": "implement", "spec": "write a.txt carefully",
  "acceptance": ["a.txt exists"], "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"}`

func openStore(t *testing.T, root string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func setStatus(t *testing.T, root, id, status string) {
	t.Helper()
	if err := openStore(t, root).SetTaskStatus(context.Background(), id, status, "", "test"); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTaskEditUpdatesGoalTaskRowAndPlanTogether(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/a", store.TaskBlocked)
	r := run(t, root, nil, "task", "edit", "g1/a", "--file", writeFile(t, editedSpec))
	if r.code != ExitOK || !strings.Contains(r.stdout, "Updated g1/a.") || !strings.Contains(r.stdout, "mill run g1 --retry-blocked") {
		t.Fatalf("edit: %+v", r)
	}
	ctx := context.Background()
	s := openStore(t, root)
	tk, _ := s.GetTask(ctx, "g1/a")
	if tk.Title != "A better" || tk.Spec.Spec != "write a.txt carefully" {
		t.Errorf("task = %+v", tk)
	}
	g, _ := s.GetGoal(ctx, "g1")
	p, err := plan.Parse(g.Plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range p.Tasks {
		if pt.ID == "a" && pt.Title != "A better" {
			t.Errorf("plan task a = %+v", pt)
		}
		if pt.ID == "c" && len(pt.DependsOn) != 2 {
			t.Errorf("plan task c lost dependencies: %+v", pt)
		}
	}
	evs, err := s.TaskEvents(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		found = found || e.Kind == "task-spec-updated"
	}
	if !found {
		t.Errorf("no task-spec-updated event: %+v", evs)
	}
	if show := run(t, root, nil, "plan", "show", "g1"); !strings.Contains(show.stdout, "A better") {
		t.Errorf("plan show: %s", show.stdout)
	}
}

func TestTaskEditRunsEditorAndAppliesResult(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/a", store.TaskFailed)
	ed := filepath.Join(t.TempDir(), "ed.sh")
	script := "#!/bin/sh\nsed 's/\"title\": \"A\"/\"title\": \"Edited by editor\"/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n"
	if err := os.WriteFile(ed, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runEnv(t, root, Env{Getenv: fakeGetenv(map[string]string{"EDITOR": ed})}, "task", "edit", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("edit: %+v", r)
	}
	tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a")
	if tk.Title != "Edited by editor" {
		t.Errorf("title = %q", tk.Title)
	}
}

func TestTaskEditInvalidEditorResultKeepsFileAndChangesNothing(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/a", store.TaskBlocked)
	ed := filepath.Join(t.TempDir(), "ed.sh")
	script := "#!/bin/sh\nsed 's/\"kind\": \"implement\"/\"kind\": \"bogus\"/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n"
	if err := os.WriteFile(ed, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runEnv(t, root, Env{Getenv: fakeGetenv(map[string]string{"EDITOR": ed})}, "task", "edit", "g1/a")
	if r.code != ExitError || !strings.Contains(r.stderr, "kind must be one of") || !strings.Contains(r.stderr, "kept in ") {
		t.Fatalf("edit: %+v", r)
	}
	i := strings.Index(r.stderr, "kept in ")
	kept := strings.Fields(r.stderr[i+len("kept in "):])[0]
	defer os.Remove(kept)
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("kept file missing: %v", err)
	}
	tk, _ := openStore(t, root).GetTask(context.Background(), "g1/a")
	if tk.Kind != "implement" {
		t.Errorf("task changed: %+v", tk)
	}
}

func TestTaskEditRefusesRunningAndVerified(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	for _, st := range []string{store.TaskRunning, store.TaskVerified} {
		setStatus(t, root, "g1/a", st)
		r := run(t, root, nil, "task", "edit", "g1/a", "--file", writeFile(t, editedSpec))
		if r.code != ExitError || !strings.Contains(r.stderr, st) {
			t.Errorf("%s: %+v", st, r)
		}
	}
}

func TestTaskEditRefusesChangedIDDependenciesAndMissingChecks(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/c", store.TaskBlocked)
	base := `{"id": "c", "title": "C", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["c.txt"], "depends_on": ["a", "b"], "size": "S"}`
	cases := map[string]struct{ body, want string }{
		"id":      {strings.Replace(base, `"id": "c"`, `"id": "z"`, 1), "id must stay"},
		"deps":    {strings.Replace(base, `["a", "b"]`, `["a"]`, 1), "dependencies cannot change after approval"},
		"checks":  {strings.Replace(base, `"checks": ["true"]`, `"checks": []`, 1), "needs at least one check"},
		"unknown": {strings.Replace(base, `"size"`, `"extra": 1, "size"`, 1), "unknown field"},
	}
	for name, c := range cases {
		r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, c.body))
		if r.code != ExitError || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, base)); r.code != ExitOK {
		t.Errorf("unchanged deps should be accepted: %+v", r)
	}
}

func TestTaskEditSingleTask(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	one := `{"id": "solo", "title": "Solo", "kind": "docs", "spec": "write docs", "acceptance": [], "checks": [], "owns": ["d.md"], "size": "S"}`
	s := openStore(t, root)
	if err := s.CreateTask(context.Background(), mustSpec(t, one)); err != nil {
		t.Fatal(err)
	}
	f := writeFile(t, strings.Replace(one, "Solo", "Solo two", 1))
	r := run(t, root, nil, "task", "edit", "solo", "--file", f)
	if r.code != ExitOK || !strings.Contains(r.stdout, "Updated solo.") || !strings.Contains(r.stdout, "mill task run --file") || !strings.Contains(r.stdout, "--retry") {
		t.Fatalf("edit: %+v", r)
	}
	tk, _ := s.GetTask(context.Background(), "solo")
	if tk.Title != "Solo two" {
		t.Errorf("title = %q", tk.Title)
	}
}

func mustSpec(t *testing.T, body string) taskspec.Spec {
	t.Helper()
	sp, err := taskspec.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}
