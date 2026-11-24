package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/store"
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
	t.Parallel()
	root := goalRepo(t, rebaseScript(`{"solo.txt": "s\n"}`), "")
	addRemote(t, root)
	r := run(t, root, nil, "keep")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to do.") {
		t.Fatalf("keep: %+v", r)
	}
}

func TestKeepJSONIsOneDocumentWithoutProgress(t *testing.T) {
	t.Parallel()
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

func TestKeepRunsItsPassWhileAnotherHolderHasTheLock(t *testing.T) {
	t.Parallel()
	root := keepRepo(t, "true")
	lock, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := run(t, root, nil, "keep")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Combined check: passed") {
		t.Fatalf("keep with held lock: %+v", r)
	}
	assertNoClaims(t, root)
}

func TestKeepReportsBusyTaskAndLeavesItUnchanged(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	ctx := context.Background()
	s := openStore(t, root)
	oldChild, _ := s.GetBranch(ctx, "g2/b")
	w := startBusyAttempt(t, root, "g2/b")
	f := forge(t, bare)
	mergeReview(t, f, "g2/a")
	lock, err := runlock.Acquire(project.PathsAt(root).RunLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	r := run(t, root, nil, "keep")
	if r.code != ExitError || !strings.Contains(r.stdout, "Busy (being worked on; run `mill keep` again later):\n  g2/b  "+w.doing) {
		t.Fatalf("keep: %+v", r)
	}
	if tk, _ := s.GetTask(ctx, "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s, want landed", tk.Status)
	}
	if got, _ := s.GetBranch(ctx, "g2/b"); got.HeadSHA != oldChild.HeadSHA || got.Base != oldChild.Base || got.BaseSHA != oldChild.BaseSHA {
		t.Errorf("busy g2/b branch changed: %+v, was %+v", got, oldChild)
	}
	w.assertUntouched(t)

	r = run(t, root, nil, "keep", "--json")
	var doc struct {
		Keeper struct {
			Sync struct {
				Busy []struct {
					TaskID string `json:"task_id"`
					Doing  string `json:"doing"`
				} `json:"busy"`
			} `json:"sync"`
			Busy []json.RawMessage `json:"busy"`
		} `json:"keeper"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("keep --json: %v\n%+v", err, r)
	}
	if r.code != ExitError || len(doc.Keeper.Sync.Busy) != 1 || doc.Keeper.Sync.Busy[0].TaskID != "g2/b" ||
		doc.Keeper.Sync.Busy[0].Doing != w.doing || doc.Keeper.Busy == nil {
		t.Fatalf("keep --json: %+v", r)
	}
	w.assertUntouched(t)
	assertNoClaims(t, root)
}

func TestKeepWatchRepeatsPassesUntilCancelled(t *testing.T) {
	t.Parallel()
	root := keepRepo(t, "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	var got []time.Duration
	wait := func(ctx context.Context, d time.Duration) bool {
		got = append(got, d)
		waits++
		if waits == 2 {
			cancel()
			return false
		}
		return true
	}

	var out, errb bytes.Buffer
	code := Run(ctx, Env{Stdout: &out, Stderr: &errb, Dir: root, KeepWait: wait}, []string{"keep", "--watch", "--interval", "10s"})
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
	t.Parallel()
	root := keepRepo(t, "true")
	r := run(t, root, nil, "keep", "--watch", "--interval", "5s")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "at least 10s") {
		t.Fatalf("keep --interval 5s: %+v", r)
	}
}

func TestKeepCombinedReportsCulpritAndPartner(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	root := keepRepo(t, "true")
	r := run(t, root, nil, "keep", "nope")
	if r.code != ExitError || !strings.Contains(r.stderr, `unknown goal "nope"`) || !strings.Contains(r.stderr, "g1, g2") {
		t.Fatalf("keep nope: %+v", r)
	}
}

func TestHelpListsKeep(t *testing.T) {
	t.Parallel()
	r := run(t, t.TempDir(), nil, "help")
	if !strings.Contains(r.stdout, "keep ") {
		t.Errorf("help lacks keep:\n%s", r.stdout)
	}
}

func TestKeepReusesCombinedResultUnlessCombinedFlagForcesIt(t *testing.T) {
	t.Parallel()
	counter := filepath.Join(t.TempDir(), "gate-runs")
	root := keepRepo(t, "echo x >> "+counter)
	gateRuns := func() int {
		b, err := os.ReadFile(counter)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(b), "x\n")
	}
	type doc struct {
		Combined struct {
			Ran    bool `json:"ran"`
			Passed bool `json:"passed"`
			Cached bool `json:"cached"`
		} `json:"combined"`
	}
	keepJSON := func(args ...string) doc {
		t.Helper()
		r := run(t, root, nil, append([]string{"keep", "--json"}, args...)...)
		if r.code != ExitOK {
			t.Fatalf("keep %v: %+v", args, r)
		}
		var d doc
		if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
		}
		return d
	}

	before := gateRuns()
	if d := keepJSON(); !d.Combined.Ran || !d.Combined.Passed || d.Combined.Cached {
		t.Fatalf("first keep = %+v", d)
	}
	first := gateRuns()
	if first == before {
		t.Fatal("first keep did not run the gate")
	}

	d := keepJSON()
	if !d.Combined.Passed || !d.Combined.Cached {
		t.Errorf("second keep = %+v, want cached green", d)
	}
	if n := gateRuns(); n != first {
		t.Errorf("second keep ran the gate (%d -> %d runs)", first, n)
	}
	r := run(t, root, nil, "keep")
	if r.code != ExitOK || !strings.Contains(r.stdout, "cached") {
		t.Errorf("summary lacks cached note: %+v", r)
	}
	if n := gateRuns(); n != first {
		t.Errorf("plain keep ran the gate (%d -> %d runs)", first, n)
	}

	d = keepJSON("--combined")
	if !d.Combined.Ran || d.Combined.Cached {
		t.Errorf("keep --combined = %+v, want a fresh run", d)
	}
	if n := gateRuns(); n <= first {
		t.Errorf("keep --combined did not run the gate (%d runs)", n)
	}
}
