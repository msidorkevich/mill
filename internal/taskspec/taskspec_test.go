package taskspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `{
  "id": "add-reverse",
  "title": "Add Reverse",
  "kind": "implement",
  "spec": "Add func Reverse(s string) string.",
  "acceptance": ["Reverse(\"abc\") == \"cba\""],
  "checks": ["go test ./..."],
  "owns": ["reverse.go"]
}`

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "add-reverse" || s.Size != "S" || len(s.Checks) != 1 {
		t.Fatalf("spec = %+v", s)
	}
}

func TestDefaultsForOptionalLists(t *testing.T) {
	s, err := Parse([]byte(`{"id":"d","title":"Docs","kind":"docs","spec":"write docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Acceptance == nil || s.Checks == nil || s.Owns == nil {
		t.Fatalf("nil lists: %+v", s)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct{ name, json, want string }{
		{"bad id", `{"id":"Bad_ID","title":"t","kind":"docs","spec":"s"}`, "lowercase slug"},
		{"no title", `{"id":"a","title":" ","kind":"docs","spec":"s"}`, "title"},
		{"bad kind", `{"id":"a","title":"t","kind":"refactor","spec":"s"}`, "kind"},
		{"no spec", `{"id":"a","title":"t","kind":"docs","spec":""}`, "spec"},
		{"implement without acceptance", `{"id":"a","title":"t","kind":"implement","spec":"s"}`, "acceptance"},
		{"empty acceptance item", `{"id":"a","title":"t","kind":"docs","spec":"s","acceptance":[""]}`, "acceptance[0]"},
		{"empty check", `{"id":"a","title":"t","kind":"docs","spec":"s","checks":[" "]}`, "checks[0]"},
		{"size L", `{"id":"a","title":"t","kind":"docs","spec":"s","size":"L"}`, "split"},
		{"bad size", `{"id":"a","title":"t","kind":"docs","spec":"s","size":"XL"}`, "size"},
		{"deps", `{"id":"a","title":"t","kind":"docs","spec":"s","depends_on":["b"]}`, "depends_on"},
		{"unknown field", `{"id":"a","title":"t","kind":"docs","spec":"s","priority":1}`, "unknown field"},
		{"trailing", `{"id":"a","title":"t","kind":"docs","spec":"s"} {}`, "trailing"},
		{"not json", `nope`, "parse task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.json)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(p, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}
