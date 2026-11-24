package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// statsModelsRepo is an initialised mill repo with an empty store.
func statsModelsRepo(t *testing.T) string {
	t.Helper()
	root := gitRepo(t)
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	return root
}

// statsModelsSeed adds tasks to the repo's store: two S/low tasks first run on sonnet (one
// verified on attempt 1, one landed after two attempts) and one M/high task on opus that is
// blocked.
func statsModelsSeed(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type att struct {
		model string
		cost  float64
	}
	add := func(id, size, complexity, status string, atts ...att) {
		sp := taskspec.Spec{ID: id, Title: id, Kind: "implement", Spec: "s", Size: size, Complexity: complexity}
		if err := s.CreateTask(ctx, sp); err != nil {
			t.Fatal(err)
		}
		for i, a := range atts {
			aid, err := s.StartAttempt(ctx, id, i+1, store.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetAttemptModel(ctx, aid, a.model, "test"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishAttempt(ctx, aid, "ok", budget.Usage{CostUSD: a.cost}, false, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetTaskStatus(ctx, id, status, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "S", "low", store.TaskVerified, att{"sonnet", 1})
	add("b", "S", "low", store.TaskLanded, att{"sonnet", 2}, att{"opus", 3})
	add("c", "M", "high", store.TaskBlocked, att{"opus", 4})
}

func TestStatsModelsPrintsOneRowPerGroup(t *testing.T) {
	t.Parallel()
	root := statsModelsRepo(t)
	statsModelsSeed(t, root)
	r := run(t, root, nil, "stats", "models")
	if r.code != ExitOK {
		t.Fatalf("stats models: %+v", r)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header and 2 rows, got:\n%s", r.stdout)
	}
	for _, h := range []string{"SIZE", "COMPLEXITY", "MODEL", "TASKS", "1ST-PASS", "AVG ATTEMPTS", "AVG $"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header %q lacks %q", lines[0], h)
		}
	}
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "S low sonnet 2 50% 1.50 3.00" {
		t.Errorf("row 1 = %q", lines[1])
	}
	if f := strings.Fields(lines[2]); strings.Join(f, " ") != "M high opus 1 0% 1.00 4.00" {
		t.Errorf("row 2 = %q", lines[2])
	}
}

func TestStatsModelsJSONPrintsTheSameData(t *testing.T) {
	t.Parallel()
	root := statsModelsRepo(t)
	statsModelsSeed(t, root)
	r := run(t, root, nil, "stats", "models", "--json")
	if r.code != ExitOK {
		t.Fatalf("stats models --json: %+v", r)
	}
	var doc struct {
		Schema string            `json:"schema"`
		Models []store.ModelStat `json:"models"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	want := []store.ModelStat{
		{Size: "S", Complexity: "low", Model: "sonnet", Tasks: 2, FirstAttemptPass: 0.5, AvgAttempts: 1.5, AvgCostUSD: 3},
		{Size: "M", Complexity: "high", Model: "opus", Tasks: 1, FirstAttemptPass: 0, AvgAttempts: 1, AvgCostUSD: 4},
	}
	if doc.Schema != "mill/stats-models@1" || len(doc.Models) != len(want) {
		t.Fatalf("doc = %+v", doc)
	}
	for i := range want {
		if doc.Models[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, doc.Models[i], want[i])
		}
	}
}

func TestStatsModelsEmptyStorePrintsMessage(t *testing.T) {
	t.Parallel()
	root := statsModelsRepo(t)
	r := run(t, root, nil, "stats", "models")
	if r.code != ExitOK || strings.TrimSpace(r.stdout) != "no tasks with attempts yet" {
		t.Fatalf("stats models on empty store: %+v", r)
	}
	r = run(t, root, nil, "stats", "models", "--json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"models": []`) {
		t.Fatalf("stats models --json on empty store: %+v", r)
	}
}

func TestStatsModelsUnknownOrMissingSubcommandIsUsageError(t *testing.T) {
	t.Parallel()
	root := statsModelsRepo(t)
	for _, args := range [][]string{{"stats"}, {"stats", "nope"}, {"stats", "models", "extra"}, {"stats", "models", "--bogus"}} {
		if r := run(t, root, nil, args...); r.code != ExitUsage {
			t.Errorf("%v: code = %d, want %d (%+v)", args, r.code, ExitUsage, r)
		}
	}
	if r := run(t, root, nil, "stats", "--help"); r.code != ExitOK || !strings.Contains(r.stdout, "mill stats models") {
		t.Errorf("stats --help: %+v", r)
	}
}

func TestStatsModelsHelpListsStats(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "help")
	if r.code != ExitOK || !strings.Contains(r.stdout, "\n  stats ") {
		t.Fatalf("help: %+v", r)
	}
}
