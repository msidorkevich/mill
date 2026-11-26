package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/store"
)

const taskModelsSpec = `{"id": "solo", "title": "Solo", "kind": "docs", "spec": "write docs", "acceptance": [], "checks": [], "owns": ["d.md"], "size": "S"}`

// taskModelsRepo returns a repo with task "solo" holding three attempts: an implementer on
// sonnet, a retry on opus with a reason, and a verifier with no recorded model.
func taskModelsRepo(t *testing.T) string {
	t.Helper()
	root := planRepo(t)
	ctx := context.Background()
	s := openStore(t, root)
	if err := s.CreateTask(ctx, mustSpec(t, taskModelsSpec)); err != nil {
		t.Fatal(err)
	}
	id1, err := s.StartAttempt(ctx, "solo", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptModel(ctx, id1, "sonnet", "mapped from complexity medium"); err != nil {
		t.Fatal(err)
	}
	id2, err := s.StartAttempt(ctx, "solo", 2, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptModel(ctx, id2, "opus", "escalated after attempt 1 failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(ctx, "solo", 3, "verifier"); err != nil {
		t.Fatal(err)
	}
	return root
}

// finishTaskModelsAttempts ends the running attempt of taskModelsRepo, so `task edit` can
// claim the task.
func finishTaskModelsAttempts(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, root)
	as, err := s.Attempts(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.Status != store.AttemptRunning {
			continue
		}
		if err := s.FinishAttempt(ctx, a.ID, store.AttemptAbandoned, budget.Usage{}, false, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func taskModelsLine(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.Join(strings.Fields(l), " ")
		}
	}
	return ""
}

func TestTaskShowModelListsRoleModelAndReasonPerAttempt(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	r := run(t, root, nil, "task", "show", "solo")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	if got := taskModelsLine(r.stdout, "2 "); !strings.HasPrefix(got, "2 implementer opus escalated after attempt 1 failed") {
		t.Errorf("attempt 2 line = %q\n%s", got, r.stdout)
	}
	if got := taskModelsLine(r.stdout, "1 "); !strings.HasPrefix(got, "1 implementer sonnet mapped from complexity medium") {
		t.Errorf("attempt 1 line = %q", got)
	}
	if got := taskModelsLine(r.stdout, "complexity"); got != "complexity medium" {
		t.Errorf("complexity line = %q", got)
	}
	if got := taskModelsLine(r.stdout, "model"); got != "model sonnet (mapping S/medium)" {
		t.Errorf("model line = %q", got)
	}
}

func TestTaskShowModelShowsOverrideAndComplexityReason(t *testing.T) {
	t.Parallel()
	root := planRepo(t)
	spec := strings.Replace(taskModelsSpec, `"size": "S"`, `"size": "S", "complexity": "high", "complexity_reason": "touches the store", "model": "opus"`, 1)
	if err := openStore(t, root).CreateTask(context.Background(), mustSpec(t, spec)); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "task", "show", "solo")
	if got := taskModelsLine(r.stdout, "complexity"); got != "complexity high (touches the store)" {
		t.Errorf("complexity line = %q\n%s", got, r.stdout)
	}
	if got := taskModelsLine(r.stdout, "model"); got != "model opus (task override)" {
		t.Errorf("model line = %q", got)
	}
}

func TestTaskShowModelDashWhenAttemptHasNoModel(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	r := run(t, root, nil, "task", "show", "solo")
	if got := taskModelsLine(r.stdout, "3 "); !strings.HasPrefix(got, "3 verifier - - ") {
		t.Errorf("attempt 3 line = %q\n%s", got, r.stdout)
	}
}

func TestTaskShowModelJSONCarriesModelAndReason(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	r := run(t, root, nil, "task", "show", "solo", "--json")
	var v struct {
		Task struct {
			Complexity string `json:"complexity"`
			Model      string `json:"model"`
		} `json:"task"`
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || len(v.Attempts) != 3 {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if v.Task.Complexity != "medium" {
		t.Errorf("task = %+v", v.Task)
	}
	if v.Attempts[1]["model"] != "opus" || v.Attempts[1]["model_reason"] != "escalated after attempt 1 failed" {
		t.Errorf("attempt 2 = %v", v.Attempts[1])
	}
}

func TestTaskEditModelSetsModelAndComplexity(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	finishTaskModelsAttempts(t, root)
	body := strings.Replace(taskModelsSpec, `"size": "S"`, `"size": "S", "complexity": "high", "complexity_reason": "hard", "model": "opus"`, 1)
	r := run(t, root, nil, "task", "edit", "solo", "--file", writeFile(t, body))
	if r.code != ExitOK {
		t.Fatalf("edit: %+v", r)
	}
	tk, err := openStore(t, root).GetTask(context.Background(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Model != "opus" || tk.Complexity != "high" || tk.ComplexityReason != "hard" {
		t.Errorf("task = %+v", tk.Spec)
	}
}

func TestTaskEditModelRefusesInvalidComplexityAndKeepsFile(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	finishTaskModelsAttempts(t, root)
	body := strings.Replace(taskModelsSpec, `"size": "S"`, `"size": "S", "complexity": "x"`, 1)
	f := writeFile(t, body)
	r := run(t, root, nil, "task", "edit", "solo", "--file", f)
	if r.code != ExitError || !strings.Contains(r.stderr, "complexity must be one of") {
		t.Fatalf("edit: %+v", r)
	}
	if data, err := os.ReadFile(f); err != nil || string(data) != body {
		t.Errorf("file changed or missing: %v %q", err, data)
	}
	tk, _ := openStore(t, root).GetTask(context.Background(), "solo")
	if tk.Complexity != "medium" {
		t.Errorf("complexity = %q, want unchanged", tk.Complexity)
	}
}

func TestTaskShowModelNamesNextAttemptModelAndWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, extra, model, why string
	}{
		{"mapping", `"size": "M", "complexity": "high", "complexity_reason": "hard"`, "opus", "mapping M/high"},
		{"override", `"size": "M", "model": "my-model"`, "my-model", "task override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := planRepo(t)
			spec := strings.Replace(taskModelsSpec, `"size": "S"`, tc.extra, 1)
			if err := openStore(t, root).CreateTask(context.Background(), mustSpec(t, spec)); err != nil {
				t.Fatal(err)
			}
			r := run(t, root, nil, "task", "show", "solo")
			want := "model " + tc.model + " (" + tc.why + ")"
			if got := taskModelsLine(r.stdout, "model"); r.code != ExitOK || got != want {
				t.Errorf("model line = %q, want %q\n%+v", got, want, r)
			}
			r = run(t, root, nil, "task", "show", "solo", "--json")
			var v struct {
				NextModel    string `json:"next_model"`
				NextModelWhy string `json:"next_model_why"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.NextModel != tc.model || v.NextModelWhy != tc.why {
				t.Errorf("json = %+v, %v\n%s", v, err, r.stdout)
			}
		})
	}
}

func TestTaskShowModelEscalatesAfterFailedAttempt(t *testing.T) {
	t.Parallel()
	root := taskModelsRepo(t)
	finishTaskModelsAttempts(t, root)
	if err := openStore(t, root).RequestChanges(context.Background(), "solo", 2, []map[string]string{}, "fix it"); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "task", "show", "solo")
	if got := taskModelsLine(r.stdout, "model"); got != "model opus (escalated after attempt 2 failed)" {
		t.Errorf("model line = %q\n%+v", got, r)
	}
}
