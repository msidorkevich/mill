package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pipeline"
)

// showDepsJSON is the part of `task show --json` these tests read.
type showDepsJSON struct {
	DependsOn []string `json:"depends_on"`
	DepsAdded []struct {
		At    time.Time `json:"at"`
		Added []string  `json:"added"`
	} `json:"deps_added"`
}

// addDepToB adds g1/a to the pending goal task g1/b through task edit --file.
func addDepToB(t *testing.T, root string) {
	t.Helper()
	if r := run(t, root, nil, "task", "edit", "g1/b", "--file", writeFile(t, specB(`["a"]`))); r.code != ExitOK {
		t.Fatalf("task edit: %+v", r)
	}
}

func TestTaskShowListsGoalTaskDependencies(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")

	r := run(t, root, nil, "task", "show", "g1/c")
	if r.code != ExitOK || !strings.Contains(r.stdout, "  depends   a, b\n") {
		t.Fatalf("task show g1/c: %+v", r)
	}
	if strings.Contains(r.stdout, "dependencies added") {
		t.Errorf("task show g1/c prints an added-dependencies line without an edit:\n%s", r.stdout)
	}
}

func TestTaskShowPrintsAddedDependenciesLine(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	addDepToB(t, root)

	evs, err := openStore(t, root).TaskEvents(context.Background(), "g1/b")
	if err != nil {
		t.Fatal(err)
	}
	var at time.Time
	for _, e := range evs {
		if e.Kind == "task-deps-added" {
			at = e.At
		}
	}
	if at.IsZero() {
		t.Fatalf("no task-deps-added event: %+v", evs)
	}

	r := run(t, root, nil, "task", "show", "g1/b")
	want := "  dependencies added " + pipeline.FormatResetTime(at, at) + ": a\n"
	if r.code != ExitOK || !strings.Contains(r.stdout, "  depends   a\n") || !strings.Contains(r.stdout, want) {
		t.Fatalf("task show g1/b: %+v\nwant line %q", r, want)
	}
}

func TestTaskShowJSONCarriesDependsOnAndDepsAdded(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")
	addDepToB(t, root)

	r := run(t, root, nil, "task", "show", "g1/b", "--json")
	if r.code != ExitOK {
		t.Fatalf("task show --json: %+v", r)
	}
	var v showDepsJSON
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("task show --json: %v\n%s", err, r.stdout)
	}
	if !reflect.DeepEqual(v.DependsOn, []string{"g1/a"}) {
		t.Errorf("depends_on = %v, want [g1/a]", v.DependsOn)
	}
	if len(v.DepsAdded) != 1 || !reflect.DeepEqual(v.DepsAdded[0].Added, []string{"g1/a"}) || v.DepsAdded[0].At.IsZero() {
		t.Errorf("deps_added = %+v, want one entry adding g1/a", v.DepsAdded)
	}
}

func TestTaskShowJSONOmitsDependencyFieldsWithoutDependencies(t *testing.T) {
	t.Parallel()
	root := goalRepo(t, fanInScript(`{"a.txt": "a\n"}`), "")

	r := run(t, root, nil, "task", "show", "g1/a", "--json")
	if r.code != ExitOK {
		t.Fatalf("task show --json: %+v", r)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &top); err != nil {
		t.Fatalf("task show --json: %v\n%s", err, r.stdout)
	}
	for _, k := range []string{"depends_on", "deps_added"} {
		if _, ok := top[k]; ok {
			t.Errorf("task show g1/a --json has %s without dependencies:\n%s", k, r.stdout)
		}
	}
}
