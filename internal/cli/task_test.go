package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mill/internal/gitx"
	"mill/internal/project"
)

const taskFile = `{
  "id": "hello",
  "title": "Say hello",
  "kind": "implement",
  "spec": "Create hello.txt containing hello.",
  "acceptance": ["hello.txt contains hello"]
}`

// taskRepo makes an initialized repo with a commit on main, a fake runner script and a
// task file. Returns the repo root and the task file path.
func taskRepo(t *testing.T, script string, maxAttempts int) (string, string) {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	p := project.PathsAt(root)
	cfg := `check_cmd = "grep -q hello hello.txt"
max_attempts = ` + itoa(maxAttempts) + `
worktree_root = "` + t.TempDir() + `"
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	if err := os.WriteFile(p.Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(tf, []byte(taskFile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, tf
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const passScript = `{
  "implementer": [{"output": {"summary": "wrote hello", "files_changed": ["hello.txt"], "notes": ""},
                   "files": {"hello.txt": "hello\n"},
                   "usage": [{"input_tokens": 1200, "output_tokens": 300, "cost_usd": 0.05}]}],
  "verifier": [{"output": {"verdict": "pass", "summary": "looks right", "findings": []}}]
}`

func TestTaskRunAndShow(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitOK {
		t.Fatalf("task run: %+v", r)
	}
	for _, want := range []string{"hello a1 implementer ok", "hello a1 gate      ok", "verdict   pass", "hello: verified", "Review: git -C"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	// Running the same task again needs --retry, and a verified task cannot be retried.
	if r := run(t, root, nil, "task", "run", "--file", tf); r.code != ExitError || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("second run: %+v", r)
	}
	if r := run(t, root, nil, "task", "run", "--file", tf, "--retry"); r.code != ExitError || !strings.Contains(r.stderr, "already verified") {
		t.Fatalf("retry of verified: %+v", r)
	}

	r = run(t, root, nil, "task", "show", "hello")
	if r.code != ExitOK {
		t.Fatalf("show: %+v", r)
	}
	for _, want := range []string{"status    verified", "branch    mill/t/hello", "1,500 tokens", "$0.05", "implementer", "verifier", "Last verdict: pass: looks right"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show missing %q:\n%s", want, r.stdout)
		}
	}

	r = run(t, root, nil, "task", "show", "hello", "--json")
	var v struct {
		Schema string `json:"schema"`
		Task   struct {
			Status string `json:"status"`
		} `json:"task"`
		Attempts []json.RawMessage `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.Schema != "mill/task@1" || v.Task.Status != "verified" || len(v.Attempts) != 2 {
		t.Fatalf("show --json: %v\n%s", err, r.stdout)
	}
}

func TestTaskRunBlockedThenRetry(t *testing.T) {
	script := `{
  "implementer": [
    {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"hello.txt": "bye\n"}},
    {"output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"hello.txt": "hello\n"}}
  ],
  "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]
}`
	root, tf := taskRepo(t, script, 1)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitNotVerified || !strings.Contains(r.stdout, "hello: blocked") {
		t.Fatalf("first run: %+v", r)
	}
	r = run(t, root, nil, "task", "show", "hello")
	if !strings.Contains(r.stdout, "status    blocked") || !strings.Contains(r.stdout, "Last findings:") || !strings.Contains(r.stdout, "[gate]") {
		t.Fatalf("show blocked:\n%s", r.stdout)
	}
	// The fake script is consumed per process, so the retry replays it from the start:
	// point it at a script whose first step passes.
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(passScript), 0o644); err != nil {
		t.Fatal(err)
	}
	r = run(t, root, nil, "task", "run", "--file", tf, "--retry")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello a2 implementer") {
		t.Fatalf("retry: %+v", r)
	}
}

func TestTaskRunErrors(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"task"}, ExitUsage, "Usage"},
		{[]string{"task", "bogus"}, ExitUsage, "unknown subcommand"},
		{[]string{"task", "run"}, ExitUsage, "--file is required"},
		{[]string{"task", "run", "--file", "/nonexistent.json"}, ExitError, "no such file"},
		{[]string{"task", "run", "--file", tf, "--retry"}, ExitError, "does not exist yet"},
		{[]string{"task", "show"}, ExitUsage, "Usage"},
		{[]string{"task", "show", "nope"}, ExitError, "not found"},
	}
	for _, c := range cases {
		r := run(t, root, nil, c.args...)
		if r.code != c.code || !strings.Contains(r.stdout+r.stderr, c.want) {
			t.Errorf("%v: code %d, want %d with %q\nstdout: %s\nstderr: %s", c.args, r.code, c.code, c.want, r.stdout, r.stderr)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"id":"x","title":"t","kind":"implement","spec":"s"}`), 0o644)
	if r := run(t, root, nil, "task", "run", "--file", bad); r.code != ExitError || !strings.Contains(r.stderr, "acceptance") {
		t.Fatalf("invalid task: %+v", r)
	}
}

func TestTaskRunNotInitialized(t *testing.T) {
	root := gitRepo(t)
	tf := filepath.Join(t.TempDir(), "task.json")
	os.WriteFile(tf, []byte(taskFile), 0o644)
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitError || !strings.Contains(r.stderr, "mill init") {
		t.Fatalf("not initialized: %+v", r)
	}
}
