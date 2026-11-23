package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
	"github.com/msidorkevich/mill/internal/store"
)

const streamLine = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x/main.go"}}]}}` + "\n"

// startRunning starts an implementer attempt on g1/b, 65s before the fixed clock.
func startRunning(t *testing.T, s *store.Store, estimated bool) {
	t.Helper()
	ctx := context.Background()
	id, err := s.StartAttempt(ctx, "g1/b", 2, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(log, []byte(streamLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttemptProcess(ctx, id, 1, "claude", log); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAttemptUsage(ctx, id, budget.Usage{InputTokens: 1000, OutputTokens: 234, CostUSD: 1.5}, estimated); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsViewShowsRunningAttempt(t *testing.T) {
	s := seed(t)
	startRunning(t, s, true)
	m, _ := newModel(t, s)
	// StartedAt comes from the store clock (== clock); advance the model clock.
	m.opts.Now = func() time.Time { return clock.Add(65 * time.Second) }
	m.Update(tickMsg(clock))
	press(m, "tab", "tab", "tab")
	out := m.View()
	for _, want := range []string{"g1/b", "#2", "implementer", "1m05s", "1,234", "~$1.50", "Read /x/main.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentsViewExactCostHasNoTilde(t *testing.T) {
	s := seed(t)
	startRunning(t, s, false)
	m, _ := newModel(t, s)
	press(m, "tab", "tab", "tab")
	out := m.View()
	if !strings.Contains(out, "$1.50") || strings.Contains(out, "~$") {
		t.Errorf("want exact cost $1.50:\n%s", out)
	}
}

func TestAgentsViewNothingRunning(t *testing.T) {
	m, _ := newModel(t, seed(t))
	press(m, "tab", "tab", "tab")
	if out := m.View(); !strings.Contains(out, "nothing running") {
		t.Errorf("View() lacks 'nothing running':\n%s", out)
	}
}

func TestFmtElapsedHours(t *testing.T) {
	if got := fmtElapsed(time.Hour + 2*time.Minute + 3*time.Second); got != "1h02m03s" {
		t.Errorf("fmtElapsed = %q", got)
	}
}

func TestAgentsViewSelectedTask(t *testing.T) {
	s := seed(t)
	startRunning(t, s, false)
	m, _ := newModel(t, s)
	press(m, "tab", "tab", "tab")
	if got := m.views[m.active].SelectedTask(); got != "g1/b" {
		t.Errorf("SelectedTask = %q", got)
	}
}

func TestAgentsViewShowsGateRowWithLogLastLine(t *testing.T) {
	s := seed(t)
	if err := s.SetTaskStatus(context.Background(), "g1/b", store.TaskRunning, store.PhaseGate, ""); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAttempt(context.Background(), "g1/b", 1, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(context.Background(), id, "done", budget.Usage{}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	m, _ := newModel(t, s)
	dir := filepath.Join(m.opts.Paths.Runs, "g1/b", "a1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gate-0.log"), []byte("building\nFAIL pkg\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.opts.Now = func() time.Time { return clock.Add(65 * time.Second) }
	m.Update(tickMsg(clock))
	press(m, "tab", "tab", "tab")
	out := m.View()
	for _, want := range []string{"g1/b", "#1", "gate", "1m05s", "FAIL pkg (/"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nothing running") {
		t.Errorf("claims nothing running:\n%s", out)
	}
	if got := m.views[m.active].SelectedTask(); got != "g1/b" {
		t.Errorf("SelectedTask = %q", got)
	}
}
