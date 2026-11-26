package cli

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// showJSON returns the exact output of `mill task show <id> --json`.
func showJSON(t *testing.T, root, id string) string {
	t.Helper()
	r := run(t, root, nil, "task", "show", id, "--json")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	return r.stdout
}

// replaceSpecText changes only the task's spec text in show's output.
func replaceSpecText(t *testing.T, root, id, doc, text string) string {
	t.Helper()
	tk, err := openStore(t, root).GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	old := `"spec": "` + tk.Spec.Spec + `"`
	if !strings.Contains(doc, old) {
		t.Fatalf("no %s in %s", old, doc)
	}
	return strings.Replace(doc, old, `"spec": "`+text+`"`, 1)
}

func TestTaskEditAcceptsShowJSONOutput(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/c", store.TaskBlocked)
	ctx := context.Background()
	s := openStore(t, root)
	before, err := s.GetTask(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s.TaskDeps(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	doc := replaceSpecText(t, root, "g1/c", showJSON(t, root, "g1/c"), "new text")

	r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, doc))
	if r.code != ExitOK || !strings.Contains(r.stdout, "Updated g1/c.") {
		t.Fatalf("edit: %+v", r)
	}
	if n := strings.Count(r.stdout, "Note: ignored non-editable fields:"); n != 1 {
		t.Fatalf("want one note, got %d: %s", n, r.stdout)
	}
	for _, f := range []string{"status", "reason", "created_at", "updated_at", "goal_id", "usage", "attempts", "runs_dir", "schema", "top-level depends_on"} {
		if !strings.Contains(r.stdout, f) {
			t.Errorf("note lacks %s: %s", f, r.stdout)
		}
	}
	after, err := s.GetTask(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	if after.Spec.Spec != "new text" || after.Title != before.Title || after.Status != store.TaskBlocked {
		t.Errorf("task = %+v", after)
	}
	got, err := s.TaskDeps(ctx, "g1/c")
	if err != nil || len(got) == 0 || !slices.Equal(got, deps) {
		t.Errorf("deps = %v, %v; want %v", got, err, deps)
	}
}

func TestTaskEditIDFormsAndOmittedDependencies(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/c", store.TaskBlocked)
	ctx := context.Background()
	s := openStore(t, root)
	deps, err := s.TaskDeps(ctx, "g1/c")
	if err != nil || len(deps) == 0 {
		t.Fatalf("deps = %v, %v", deps, err)
	}
	body := func(id string) string {
		return `{"id": "` + id + `", "title": "C", "kind": "implement", "spec": "x", "acceptance": ["x"], "checks": ["true"], "owns": ["c.txt"]}`
	}
	for _, id := range []string{"c", "g1/c"} {
		r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, body(id)))
		if r.code != ExitOK || strings.Contains(r.stdout, "Note:") {
			t.Errorf("%s: %+v", id, r)
		}
		got, err := s.TaskDeps(ctx, "g1/c")
		if err != nil || !slices.Equal(got, deps) {
			t.Errorf("%s: deps = %v, %v; want %v", id, got, err, deps)
		}
	}
	for _, id := range []string{"a", "g1/a", "g2/c"} {
		r := run(t, root, nil, "task", "edit", "g1/c", "--file", writeFile(t, body(id)))
		if r.code != ExitError || !strings.Contains(r.stderr, "id must stay") {
			t.Errorf("%s: %+v", id, r)
		}
	}
}

func TestTaskEditRefusesFieldsShowDoesNotPrint(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "x\n"}`), "")
	setStatus(t, root, "g1/a", store.TaskBlocked)
	doc := showJSON(t, root, "g1/a")
	for name, body := range map[string]string{
		"wrapper": strings.Replace(doc, `"schema"`, `"extra": 1, "schema"`, 1),
		"task":    strings.Replace(doc, `"title"`, `"extra": 1, "title"`, 1),
	} {
		r := run(t, root, nil, "task", "edit", "g1/a", "--file", writeFile(t, body))
		if r.code != ExitError || !strings.Contains(r.stderr, `unknown field "extra"`) {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestTaskEditSingleTaskFromShowJSONSuggestsRunnableFile(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	s := openStore(t, root)
	one := `{"id": "solo", "title": "Solo", "kind": "docs", "spec": "write docs", "owns": ["d.md"]}`
	if err := s.CreateTask(context.Background(), mustSpec(t, one)); err != nil {
		t.Fatal(err)
	}
	doc := replaceSpecText(t, root, "solo", showJSON(t, root, "solo"), "new docs")
	r := run(t, root, nil, "task", "edit", "solo", "--file", writeFile(t, doc))
	if r.code != ExitOK {
		t.Fatalf("edit: %+v", r)
	}
	_, rest, ok := strings.Cut(r.stdout, "mill task run --file ")
	if !ok {
		t.Fatalf("no next step: %s", r.stdout)
	}
	path, _, _ := strings.Cut(rest, " --retry")
	defer os.Remove(path)
	sp, err := taskspec.Load(path)
	if err != nil || sp.Spec != "new docs" {
		t.Errorf("suggested file: %+v, %v", sp, err)
	}
}
