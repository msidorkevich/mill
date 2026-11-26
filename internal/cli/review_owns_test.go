package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewShowNamesOwningTaskOfOutsideFile(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	s := openStore(t, root)
	ev := map[string]any{"attempt": 1, "files": []string{"a.go", "stray.txt"}, "owners": map[string][]string{"a.go": {"g1/b"}}}
	if _, err := s.AppendEvent(context.Background(), "owns-outside", "", "g1/a", ev); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	for _, want := range []string{"  owns-outside: a.go (owned by g1/b)\n", "  owns-outside: stray.txt\n"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, `"files"`) || strings.Contains(r.stdout, "stray.txt (owned") {
		t.Errorf("show printed raw event data or an owner for an unowned file:\n%s", r.stdout)
	}
}

func TestReviewShowRendersOldOwnsOutsideEventAsUnowned(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	s := openStore(t, root)
	ev := map[string]any{"attempt": 1, "files": []string{"stray.txt"}}
	if _, err := s.AppendEvent(context.Background(), "owns-outside", "", "g1/a", ev); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "  owns-outside: stray.txt\n") || strings.Contains(r.stdout, "owned by") {
		t.Fatalf("show: %+v", r)
	}
}

func TestReviewShowJSONKeepsOwnersInEventData(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	s := openStore(t, root)
	ev := map[string]any{"attempt": 1, "files": []string{"a.go"}, "owners": map[string][]string{"a.go": {"g1/b"}}}
	if _, err := s.AppendEvent(context.Background(), "owns-outside", "", "g1/a", ev); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a", "--json")
	var v struct {
		Events []struct {
			Kind string `json:"kind"`
			Data struct {
				Files  []string            `json:"files"`
				Owners map[string][]string `json:"owners"`
			} `json:"data"`
		} `json:"events"`
	}
	if r.code != ExitOK {
		t.Fatalf("show --json: %+v", r)
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if len(v.Events) != 1 || v.Events[0].Kind != "owns-outside" || len(v.Events[0].Data.Files) != 1 ||
		len(v.Events[0].Data.Owners["a.go"]) != 1 || v.Events[0].Data.Owners["a.go"][0] != "g1/b" {
		t.Fatalf("events = %+v\n%s", v.Events, r.stdout)
	}
}

func TestReviewOutOfScopeFindingsShownAsNotes(t *testing.T) {
	t.Parallel()
	root := verifiedGoal(t)
	s := openStore(t, root)
	ev := map[string]any{"attempt": 1,
		"findings": []map[string]any{{"source": "verifier", "file": "b.txt", "line": 3, "issue": "b.txt lacks a header"}},
		"owners":   map[string][]string{"b.txt": {"g1/b"}}}
	if _, err := s.AppendEvent(context.Background(), "verifier-out-of-scope", "", "g1/a", ev); err != nil {
		t.Fatal(err)
	}
	r := run(t, root, nil, "review", "show", "g1/a")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	want := "Notes from attempt 1:\n  verifier-out-of-scope: [verifier] b.txt:3: b.txt lacks a header (owned by g1/b)\n"
	if !strings.Contains(r.stdout, want) {
		t.Fatalf("show missing %q:\n%s", want, r.stdout)
	}
	if strings.Contains(r.stdout, `"findings"`) {
		t.Errorf("show printed raw event data:\n%s", r.stdout)
	}
	r = run(t, root, nil, "review", "show", "g1/a", "--json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"kind": "verifier-out-of-scope"`) {
		t.Fatalf("show --json: %+v", r)
	}
}
