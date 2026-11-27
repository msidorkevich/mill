package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/cistatus"
	"github.com/msidorkevich/mill/internal/hook"
)

// ciDisplayCI is the ci object of the --json output.
type ciDisplayCI struct {
	State  string             `json:"state"`
	SHA    string             `json:"sha"`
	URL    string             `json:"url"`
	Stale  bool               `json:"stale"`
	Checks []hook.StatusCheck `json:"checks"`
}

// ciDisplaySetup is a goal repo with a bare remote where g1/a and g1/b are published and
// g1/c (which needs both) is verified. It returns the root and the published sha of each
// published task.
func ciDisplaySetup(t *testing.T) (string, map[string]string) {
	t.Helper()
	root, _ := publishedGoal(t)
	if r := run(t, root, nil, "review", "approve", "g1/a", "g1/b"); r.code != ExitOK {
		t.Fatalf("approve: %+v", r)
	}
	s := openStore(t, root)
	shas := map[string]string{}
	for _, id := range []string{"g1/a", "g1/b"} {
		br, err := s.GetBranch(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if br.PublishedSHA == "" {
			t.Fatalf("%s has no published sha after approve", id)
		}
		shas[id] = br.PublishedSHA
	}
	return root, shas
}

// ciDisplaySeed appends a ci-status event for taskID through the store.
func ciDisplaySeed(t *testing.T, root, taskID, sha, state, url string, checks ...hook.StatusCheck) {
	t.Helper()
	if checks == nil {
		checks = []hook.StatusCheck{}
	}
	data := map[string]any{"sha": sha, "state": state, "url": url, "checks": checks}
	if _, err := openStore(t, root).AppendEvent(context.Background(), cistatus.Event, "", taskID, data); err != nil {
		t.Fatal(err)
	}
}

// ciDisplayColumn returns the CI column of taskID's row in the `mill review` table.
func ciDisplayColumn(t *testing.T, out, taskID string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	start, end := strings.Index(lines[0], "CI"), strings.Index(lines[0], "WAITING FOR")
	if start < 0 || end < start {
		t.Fatalf("review header has no CI column before WAITING FOR:\n%s", out)
	}
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, taskID+" ") {
			return strings.TrimSpace(l[start:end])
		}
	}
	t.Fatalf("no row for %s:\n%s", taskID, out)
	return ""
}

// ciDisplayJSON runs args (which end in --json) and returns the raw ci object of each task:
// the document's own for review show and task show, each task's for review.
func ciDisplayJSON(t *testing.T, root string, args ...string) map[string]json.RawMessage {
	t.Helper()
	r := run(t, root, nil, args...)
	if r.code != ExitOK {
		t.Fatalf("%v: %+v", args, r)
	}
	out := map[string]json.RawMessage{}
	if args[0] == "review" && args[1] == "--json" {
		var doc struct {
			Tasks []struct {
				ID string          `json:"id"`
				CI json.RawMessage `json:"ci"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, r.stdout)
		}
		for _, tk := range doc.Tasks {
			if tk.CI == nil {
				t.Fatalf("%v: task %s has no ci key:\n%s", args, tk.ID, r.stdout)
			}
			out[tk.ID] = tk.CI
		}
		return out
	}
	var doc struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
		CI json.RawMessage `json:"ci"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, r.stdout)
	}
	if doc.CI == nil {
		t.Fatalf("%v: no ci key:\n%s", args, r.stdout)
	}
	out[doc.Task.ID] = doc.CI
	return out
}

func ciDisplayDecode(t *testing.T, raw json.RawMessage) ciDisplayCI {
	t.Helper()
	var ci ciDisplayCI
	if string(raw) == "null" {
		t.Fatal("ci is null, want an object")
	}
	if err := json.Unmarshal(raw, &ci); err != nil {
		t.Fatalf("ci %s: %v", raw, err)
	}
	return ci
}

func TestCIDisplayReviewColumnShowsStateOfPublishedHead(t *testing.T) {
	t.Parallel()
	root, shas := ciDisplaySetup(t)
	// A record on the unpublished task is not shown.
	ciDisplaySeed(t, root, "g1/c", shas["g1/a"], "success", "")

	for _, state := range []string{"success", "pending", "failure", "none"} {
		ciDisplaySeed(t, root, "g1/a", shas["g1/a"], state, "https://example.test/pr/1")
		r := run(t, root, nil, "review")
		if r.code != ExitOK {
			t.Fatalf("review: %+v", r)
		}
		if got := ciDisplayColumn(t, r.stdout, "g1/a"); got != state {
			t.Errorf("g1/a CI = %q, want %q:\n%s", got, state, r.stdout)
		}
		if got := ciDisplayColumn(t, r.stdout, "g1/b"); got != "-" {
			t.Errorf("g1/b (no record) CI = %q, want -:\n%s", got, r.stdout)
		}
		if got := ciDisplayColumn(t, r.stdout, "g1/c"); got != "-" {
			t.Errorf("g1/c (unpublished) CI = %q, want -:\n%s", got, r.stdout)
		}
		cis := ciDisplayJSON(t, root, "review", "--json")
		ci := ciDisplayDecode(t, cis["g1/a"])
		if ci.State != state || ci.SHA != shas["g1/a"] || ci.URL != "https://example.test/pr/1" || ci.Stale {
			t.Errorf("g1/a ci = %+v, want %s at %s", ci, state, shas["g1/a"])
		}
		if string(cis["g1/b"]) != "null" {
			t.Errorf("g1/b ci = %s, want null", cis["g1/b"])
		}
	}
}

func TestCIDisplayRecordOfOlderHeadIsStaleEverywhere(t *testing.T) {
	t.Parallel()
	root, _ := ciDisplaySetup(t)
	old := strings.Repeat("1234abcd", 5)
	ciDisplaySeed(t, root, "g1/a", old, "success", "https://example.test/pr/1")
	want := "stale (1234abcd)"

	r := run(t, root, nil, "review")
	if r.code != ExitOK {
		t.Fatalf("review: %+v", r)
	}
	if got := ciDisplayColumn(t, r.stdout, "g1/a"); got != want {
		t.Errorf("review CI = %q, want %q:\n%s", got, want, r.stdout)
	}
	for _, args := range [][]string{{"review", "show", "g1/a"}, {"task", "show", "g1/a"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		for _, w := range []string{"\nCI:\n", "  state  " + want + "\n", "  sha    1234abcd\n", "  url    https://example.test/pr/1\n"} {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("%v missing %q:\n%s", args, w, r.stdout)
			}
		}
	}
	for _, args := range [][]string{{"review", "--json"}, {"review", "show", "g1/a", "--json"}, {"task", "show", "g1/a", "--json"}} {
		ci := ciDisplayDecode(t, ciDisplayJSON(t, root, args...)["g1/a"])
		if !ci.Stale || ci.SHA != old || ci.State != "success" {
			t.Errorf("%v: ci = %+v, want stale success at %s", args, ci, old)
		}
	}
}

func TestCIDisplayShowsListFailingChecksWithURLs(t *testing.T) {
	t.Parallel()
	root, shas := ciDisplaySetup(t)
	checks := []hook.StatusCheck{
		{Name: "unit", State: "failure", URL: "https://example.test/checks/unit", Summary: "2 failed"},
		{Name: "lint", State: "success", URL: "https://example.test/checks/lint"},
		{Name: "e2e", State: "failure", URL: "https://example.test/checks/e2e"},
	}
	ciDisplaySeed(t, root, "g1/a", shas["g1/a"], "failure", "https://example.test/pr/1", checks...)

	for _, args := range [][]string{{"review", "show", "g1/a"}, {"task", "show", "g1/a"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		for _, w := range []string{"  state  failure\n", "  sha    " + shas["g1/a"][:8] + "\n",
			"\n  unit  https://example.test/checks/unit\n", "\n  e2e  https://example.test/checks/e2e\n"} {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("%v missing %q:\n%s", args, w, r.stdout)
			}
		}
		if strings.Contains(r.stdout, "checks/lint") {
			t.Errorf("%v lists the passing check:\n%s", args, r.stdout)
		}
	}
	for _, args := range [][]string{{"review", "--json"}, {"review", "show", "g1/a", "--json"}, {"task", "show", "g1/a", "--json"}} {
		ci := ciDisplayDecode(t, ciDisplayJSON(t, root, args...)["g1/a"])
		if ci.State != "failure" || ci.Stale || len(ci.Checks) != 3 || ci.Checks[0].URL != checks[0].URL || ci.Checks[2].Name != "e2e" {
			t.Errorf("%v: ci = %+v", args, ci)
		}
	}
}

func TestCIDisplayJSONIsNullWithoutRecord(t *testing.T) {
	t.Parallel()
	root, _ := ciDisplaySetup(t)
	for _, args := range [][]string{{"review", "--json"}, {"review", "show", "g1/b", "--json"}, {"task", "show", "g1/b", "--json"}, {"task", "show", "g1/c", "--json"}} {
		cis := ciDisplayJSON(t, root, args...)
		for id, raw := range cis {
			if string(raw) != "null" {
				t.Errorf("%v: %s ci = %s, want null", args, id, raw)
			}
		}
	}
	for _, args := range [][]string{{"review", "show", "g1/b"}, {"task", "show", "g1/b"}} {
		r := run(t, root, nil, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		if strings.Contains(r.stdout, "CI:") {
			t.Errorf("%v shows a CI block without a record:\n%s", args, r.stdout)
		}
	}
}
