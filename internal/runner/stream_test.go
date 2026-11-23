package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestActionsListsEveryActionInLastActionFormat(t *testing.T) {
	tool := func(name, input string) string {
		return `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	text := func(s string) string {
		return `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"` + s + `"}]}}` + "\n"
	}
	stream := `{"type":"system","subtype":"init"}` + "\n" +
		text("Looking  at\\nthe code") +
		tool("Read", `{"file_path":"internal/x.go"}`) +
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}` + "\n" +
		tool("Bash", `{"command":"go test ./..."}`) +
		`not json` + "\n" +
		text("All done") +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","na`
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Actions(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Looking at the code", "Read internal/x.go", "Bash go test ./...", "All done"}
	if !slices.Equal(got, want) {
		t.Fatalf("Actions = %q, want %q", got, want)
	}
	last, err := LastAction(path)
	if err != nil || last != want[len(want)-1] {
		t.Fatalf("LastAction = %q, %v; want the last of Actions %q", last, err, want[len(want)-1])
	}
}

func TestActionsOfEmptyStreamIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"system","subtype":"init"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Actions(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("Actions = %q, %v; want none", got, err)
	}
}

func TestActionsMissingFileFails(t *testing.T) {
	if _, err := Actions(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: want an error")
	}
}
