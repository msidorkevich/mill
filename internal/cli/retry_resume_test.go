package cli

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// retryResumeScript has the implementer spend 0.4 USD. Every mill command builds a new fake
// runner, so the retry run could play the implementer step again if it called for one.
const retryResumeScript = `{"tasks": {"g1/a": {
  "implementer": [{"output": {"summary": "s", "files_changed": ["a.txt"], "notes": ""},
                   "files": {"a.txt": "a\n"}, "usage": [{"cost_usd": 0.4}]}],
  "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]
}}}`

// retryResumeState returns the stored task g1/a and its attempts as "<role> a<n> <status>".
func retryResumeState(t *testing.T, root string) (store.Task, []string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, project.PathsAt(root).DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tk, err := s.GetTask(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(ctx, "g1/a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range attempts {
		got = append(got, a.Role+" a"+itoa(a.N)+" "+a.Status)
	}
	return tk, got
}

func TestRetryBlockedResumesAtVerifier(t *testing.T) {
	t.Parallel()
	root := schedRepo(t, retryResumeScript, 1)
	configPath := project.PathsAt(root).Config
	base, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// An attempt may use 0.5 and the verifier half of that. A task budget of 0.6 covers the
	// implementer's attempt, but the 0.2 left after it does not cover the verifier's 0.25.
	writeBudget := func(task string) {
		t.Helper()
		cfg := strings.Replace(string(base), "[budget.task]\nusd = 0.0\n", `[budget]
verifier_share = 0.5
[budget.size.S]
attempt = { usd = 0.5 }
[budget.task]
usd = `+task+"\n", 1)
		if cfg == string(base) {
			t.Fatalf("no task budget to replace in %s:\n%s", configPath, base)
		}
		if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBudget("0.6")
	approvePlan(t, root, "g1", schedPlan(t, nil, "Resume", schedTask{id: "a", file: "a.txt"}))
	env := Env{SchedulerPoll: groupBPoll, TaskRunPoll: groupBPoll, Home: t.TempDir()}

	first := runEnv(t, root, env, "run", "g1")
	if first.code != ExitNotVerified {
		t.Fatalf("first run: %+v", first)
	}
	if !strings.Contains(first.stdout, "g1/a a1 gate      ok") {
		t.Errorf("first run did not pass the gate:\n%s", first.stdout)
	}
	tk, attempts := retryResumeState(t, root)
	if tk.Status != store.TaskBlocked || tk.Phase != store.PhaseVerify || !strings.Contains(tk.Reason, "the verifier may use") {
		t.Fatalf("task after first run = %s in %s (%q), want blocked in verify", tk.Status, tk.Phase, tk.Reason)
	}
	if !strings.Contains(first.stdout, "Blocked: g1/a blocked (") || !strings.Contains(first.stdout, "the verifier may use") {
		t.Errorf("first run does not report why g1/a is blocked:\n%s", first.stdout)
	}
	implemented := []string{"implementer a1 ok"}
	if !slices.Equal(attempts, implemented) {
		t.Fatalf("attempts after first run = %q, want %q", attempts, implemented)
	}

	writeBudget("2.0")
	retry := runEnv(t, root, env, "run", "g1", "--retry-blocked")
	if retry.code != ExitOK {
		t.Fatalf("retry run: %+v", retry)
	}
	if !strings.Contains(retry.stdout, "resuming at the verifier") {
		t.Errorf("retry run did not resume at the verifier:\n%s", retry.stdout)
	}
	// The pipeline logs a line and records an attempt for every call to the fake runner.
	if strings.Contains(retry.stdout, " implementer ") {
		t.Errorf("retry run called the implementer:\n%s", retry.stdout)
	}
	tk, attempts = retryResumeState(t, root)
	if tk.Status != store.TaskVerified {
		t.Errorf("task after retry run = %s (%q), want verified", tk.Status, tk.Reason)
	}
	if want := append(implemented, "verifier a1 ok"); !slices.Equal(attempts, want) {
		t.Errorf("attempts after retry run = %q, want %q", attempts, want)
	}
}
