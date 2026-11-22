package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
)

func TestFakePlaysStepsInOrderPerRole(t *testing.T) {
	f := NewFake(Script{
		RoleImplementer: {
			{Output: []byte(`{"summary":"first"}`)},
			{Output: []byte(`{"summary":"second"}`)},
		},
		RoleVerifier: {{Output: []byte(`{"verdict":"pass"}`)}},
	})
	ctx := context.Background()
	for _, want := range []string{`{"summary":"first"}`, `{"summary":"second"}`} {
		res, err := f.Run(ctx, Request{Role: RoleImplementer})
		if err != nil || res.Status != StatusOK || string(res.Output) != want {
			t.Fatalf("Run = %+v, %v; want output %s", res, err, want)
		}
	}
	res, err := f.Run(ctx, Request{Role: RoleVerifier, Prompt: "check it"})
	if err != nil || string(res.Output) != `{"verdict":"pass"}` {
		t.Fatalf("verifier = %+v, %v", res, err)
	}
	if _, err := f.Run(ctx, Request{Role: RoleImplementer}); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("err = %v, want ErrScriptExhausted", err)
	}
	calls := f.Calls()
	if len(calls) != 4 || calls[2].Prompt != "check it" {
		t.Fatalf("calls recorded wrong: %+v", calls)
	}
}

func TestFakeReportsUsageIncrements(t *testing.T) {
	f := NewFake(Script{RoleImplementer: {{
		Usage: []budget.Usage{{InputTokens: 100, CostUSD: 0.1}, {OutputTokens: 50, CostUSD: 0.2}},
	}}})
	var seen []budget.Usage
	res, err := f.Run(context.Background(), Request{
		Role:    RoleImplementer,
		OnUsage: func(d budget.Usage) { seen = append(seen, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].InputTokens != 100 || seen[1].OutputTokens != 50 {
		t.Fatalf("increments = %+v", seen)
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 50 {
		t.Fatalf("total usage = %+v", res.Usage)
	}
	if string(res.Output) != "{}" {
		t.Fatalf("default output = %s, want {}", res.Output)
	}
}

// A caller enforcing a budget cancels the context from OnUsage; the fake stops at once.
func TestFakeStopsWhenCancelledMidRun(t *testing.T) {
	f := NewFake(Script{RoleImplementer: {{
		Usage: []budget.Usage{{InputTokens: 300}, {InputTokens: 300}, {InputTokens: 300}},
	}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limit := budget.Limits{Tokens: 500}
	var spent budget.Usage
	res, err := f.Run(ctx, Request{
		Role: RoleImplementer,
		OnUsage: func(d budget.Usage) {
			spent = spent.Add(d)
			if over, _ := budget.Exceeded(spent, limit, budget.CountBillable); over {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", res.Status)
	}
	if res.Usage.InputTokens != 600 {
		t.Fatalf("usage = %d, want 600 (stopped after the second increment)", res.Usage.InputTokens)
	}
}

func TestFakeScriptedError(t *testing.T) {
	f := NewFake(Script{RoleImplementer: {{Error: "model refused"}}})
	res, err := f.Run(context.Background(), Request{Role: RoleImplementer})
	if err != nil || res.Status != StatusError || res.Error != "model refused" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestFakeWritesFiles(t *testing.T) {
	dir := t.TempDir()
	f := NewFake(Script{RoleImplementer: {{Files: map[string]string{"pkg/a.go": "package pkg\n"}}}})
	if _, err := f.Run(context.Background(), Request{Role: RoleImplementer, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pkg", "a.go"))
	if err != nil || string(got) != "package pkg\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
}

func TestFakeRefusesPathsOutsideDir(t *testing.T) {
	for _, p := range []string{"../escape.txt", "/etc/passwd", "a/../../b"} {
		f := NewFake(Script{RoleImplementer: {{Files: map[string]string{p: "x"}}}})
		if _, err := f.Run(context.Background(), Request{Role: RoleImplementer, Dir: t.TempDir()}); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestLoadFakeScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "script.json")
	data := `{"implementer":[{"output":{"summary":"ok"},"usage":[{"input_tokens":5,"cost_usd":0.01}]}]}`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFakeScript(p)
	if err != nil {
		t.Fatal(err)
	}
	steps := s.Roles[RoleImplementer]
	if len(steps) != 1 || steps[0].Usage[0].InputTokens != 5 || string(steps[0].Output) != `{"summary":"ok"}` {
		t.Fatalf("script = %+v", s)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFakeScript(p); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestLoadFakeScriptTasksKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "script.json")
	data := `{"implementer":[{"output":{"summary":"role"}}],"tasks":{"g/a":{"implementer":[{"output":{"summary":"a"}}]}}}`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFakeScript(p)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFake(s)
	ctx := context.Background()
	for _, c := range []struct{ task, want string }{{"g/a", `{"summary":"a"}`}, {"g/b", `{"summary":"role"}`}} {
		res, err := f.Run(ctx, Request{Role: RoleImplementer, TaskID: c.task})
		if err != nil || string(res.Output) != c.want {
			t.Fatalf("task %s: %s, %v; want %s", c.task, res.Output, err, c.want)
		}
	}
	// the task script is exhausted independently of the role script
	if _, err := f.Run(ctx, Request{Role: RoleImplementer, TaskID: "g/a"}); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("err = %v, want ErrScriptExhausted", err)
	}
}

func TestFakeDelayHonoursCancellation(t *testing.T) {
	f := NewFake(Script{RoleImplementer: {{DelayMS: 60000}, {DelayMS: 20}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	res, err := f.Run(ctx, Request{Role: RoleImplementer})
	if err != nil || res.Status != StatusCancelled || time.Since(start) > 5*time.Second {
		t.Fatalf("res = %+v err = %v after %s, want prompt StatusCancelled", res, err, time.Since(start))
	}
	res, err = f.Run(context.Background(), Request{Role: RoleImplementer})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("delayed step: %+v %v", res, err)
	}
}
