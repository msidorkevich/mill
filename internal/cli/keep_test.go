package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
)

const independentPlan = `{
  "goal": "Indep",
  "assumptions": [],
  "tasks": [
    {"id": "a", "title": "A", "kind": "implement", "spec": "write a.txt", "acceptance": ["a.txt exists"],
     "checks": ["test -f a.txt"], "owns": ["a.txt"], "depends_on": [], "size": "S"},
    {"id": "b", "title": "B", "kind": "implement", "spec": "write b.txt", "acceptance": ["b.txt exists"],
     "checks": ["test -f b.txt"], "owns": ["b.txt"], "depends_on": [], "size": "S"}
  ]
}`

// keepRepo is a repo with a bare remote and goal g1 whose independent tasks a and b are
// verified; checkCmd is the gate.
func keepRepo(t *testing.T, checkCmd string) string {
	t.Helper()
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	cfgPath := project.PathsAt(root).Config
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`check_cmd = "true"`), []byte("check_cmd = '"+checkCmd+"'"), 1)
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// goalRepo already created g1 (fan-in plan); a second goal holds the independent tasks.
	if r := run(t, root, nil, "plan", "--file", writePlan(t, independentPlan)); r.code != ExitOK {
		t.Fatalf("plan: %+v", r)
	}
	if r := run(t, root, nil, "plan", "approve", "g2"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	if r := run(t, root, nil, "run", "g2"); r.code != ExitOK {
		t.Fatalf("run: %+v", r)
	}
	addRemote(t, root)
	return root
}

func TestKeepPrintsSummaryAndNothingToDo(t *testing.T) {
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	addRemote(t, root)
	r := run(t, root, nil, "keep")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to do.") {
		t.Fatalf("keep: %+v", r)
	}
}

func TestKeepJSONIsOneDocumentWithoutProgress(t *testing.T) {
	root := keepRepo(t, "true")
	r := run(t, root, nil, "keep", "--json")
	if r.code != ExitOK {
		t.Fatalf("keep --json: %+v", r)
	}
	var doc struct {
		Schema   string          `json:"schema"`
		Keeper   json.RawMessage `json:"keeper"`
		Combined struct {
			Ran    bool `json:"ran"`
			Passed bool `json:"passed"`
		} `json:"combined"`
	}
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if dec.More() {
		t.Errorf("stdout holds more than one document:\n%s", r.stdout)
	}
	if doc.Schema != "mill/keep@1" || len(doc.Keeper) == 0 || string(doc.Keeper) == "null" || !doc.Combined.Ran || !doc.Combined.Passed {
		t.Errorf("document = %+v\n%s", doc, r.stdout)
	}
	var k map[string]json.RawMessage
	if err := json.Unmarshal(doc.Keeper, &k); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sync", "rebased", "resolved_rerere", "resolved_agent", "fixed", "blocked", "republished", "needs_republish", "skipped"} {
		if _, ok := k[f]; !ok {
			t.Errorf("keeper lacks %q:\n%s", f, r.stdout)
		}
	}
	var q map[string]json.RawMessage
	if err := json.Unmarshal(k["queue"], &q); err != nil {
		t.Fatalf("keeper.queue: %v\n%s", err, r.stdout)
	}
	for _, f := range []string{"published", "waiting"} {
		if v := strings.Join(strings.Fields(string(q[f])), ""); v != "[]" {
			t.Errorf("keeper.queue.%s = %s, want []", f, q[f])
		}
	}
}

func TestKeepRefusesWhileAnotherHolderHasTheLock(t *testing.T) {
	root := keepRepo(t, "true")
	lock, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := run(t, root, nil, "keep")
	if r.code != ExitError || r.stderr == "" || strings.Contains(r.stdout, "Combined check") {
		t.Fatalf("keep with held lock: %+v", r)
	}
	lock.Release()
	if r := run(t, root, nil, "keep"); r.code != ExitOK {
		t.Fatalf("keep after release: %+v", r)
	}
}

func TestKeepWatchRepeatsPassesUntilCancelled(t *testing.T) {
	root := keepRepo(t, "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	var got []time.Duration
	old := keepWait
	keepWait = func(ctx context.Context, d time.Duration) bool {
		got = append(got, d)
		waits++
		if waits == 2 {
			cancel()
			return false
		}
		return true
	}
	defer func() { keepWait = old }()

	var out, errb bytes.Buffer
	code := Run(ctx, Env{Stdout: &out, Stderr: &errb, Dir: root}, []string{"keep", "--watch", "--interval", "10s"})
	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, errb.String())
	}
	if n := strings.Count(out.String(), "Combined check:"); n < 2 {
		t.Errorf("passes printed = %d, want >= 2:\n%s", n, out.String())
	}
	if len(got) == 0 || got[0] != 10*time.Second {
		t.Errorf("waits = %v", got)
	}
}

func TestKeepRejectsShortInterval(t *testing.T) {
	root := keepRepo(t, "true")
	r := run(t, root, nil, "keep", "--watch", "--interval", "5s")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "at least 10s") {
		t.Fatalf("keep --interval 5s: %+v", r)
	}
}

func TestKeepCombinedReportsCulpritAndPartner(t *testing.T) {
	root := keepRepo(t, "! { test -f a.txt && test -f b.txt; }")
	r := run(t, root, nil, "keep", "--combined")
	if r.code != ExitOK {
		t.Fatalf("keep --combined: %+v", r)
	}
	for _, want := range []string{"Combined check: failed", "culprit: g2/b", "partner: g2/a"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestKeepUnknownGoalListsGoals(t *testing.T) {
	root := keepRepo(t, "true")
	r := run(t, root, nil, "keep", "nope")
	if r.code != ExitError || !strings.Contains(r.stderr, `unknown goal "nope"`) || !strings.Contains(r.stderr, "g1, g2") {
		t.Fatalf("keep nope: %+v", r)
	}
}

func TestHelpListsKeep(t *testing.T) {
	r := run(t, t.TempDir(), nil, "help")
	if !strings.Contains(r.stdout, "keep ") {
		t.Errorf("help lacks keep:\n%s", r.stdout)
	}
}
