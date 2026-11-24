package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/graph"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

var clock = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func spec(id string, deps ...string) taskspec.Spec {
	return taskspec.Spec{ID: id, Title: strings.ToUpper(id), Kind: "implement", Spec: "x",
		Acceptance: []string{"x"}, Checks: []string{"true"}, Owns: []string{id + ".go"},
		DependsOn: deps, Size: "S"}
}

// seed opens a store in a temp dir with g1 (approved: a <- b, a <- c) and g2 (draft).
func seed(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "mill.db"), store.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g1, err := s.CreateGoal(ctx, "Build the widget", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Plan{Goal: "Build the widget", Tasks: []taskspec.Spec{spec("a"), spec("b", "a"), spec("c", "a")}}
	if err := s.ApproveGoal(ctx, g1.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(ctx, "g1/a", store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGoal(ctx, "Polish the docs", nil); err != nil {
		t.Fatal(err)
	}
	return s
}

func newModel(t *testing.T, s Store) (*Model, *FakeRunner) {
	t.Helper()
	f := NewFakeRunner()
	dir := t.TempDir()
	m := New(Options{
		Store:    s,
		Paths:    project.PathsAt(dir),
		Runner:   f,
		Now:      func() time.Time { return clock },
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m.Update(tickMsg(clock))
	return m, f
}

// press sends a key and returns the command the model answered with.
func press(m *Model, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd = m.Update(msg)
	}
	return cmd
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func screen(m *Model) string { return ansiRE.ReplaceAllString(m.View(), "") }

func lastStart(t *testing.T, f *FakeRunner) []string {
	t.Helper()
	calls := f.Calls()
	if len(calls) == 0 {
		t.Fatal("the runner received no call")
	}
	c := calls[len(calls)-1]
	if c.Method != "Start" {
		t.Fatalf("last call is %s, want Start", c.Method)
	}
	return c.Args
}

func TestGoalsViewListsGoalsAndSelectedGoalGraph(t *testing.T) {
	t.Parallel()
	m, _ := newModel(t, seed(t))
	v := screen(m)
	for _, want := range []string{"g1", "Build the widget", "approved", "g2", "Polish the docs", "draft"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	gr := graph.Graph{
		Nodes: []graph.Node{{ID: "a", Label: "a", Status: "landed"}, {ID: "b", Label: "b", Status: "pending"}, {ID: "c", Label: "c", Status: "pending"}},
		Edges: []graph.Edge{{From: "b", To: "a"}, {From: "c", To: "a"}},
	}
	var want strings.Builder
	if err := graph.Terminal(&want, gr); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(want.String(), "\n"), "\n") {
		if !strings.Contains(v, line) {
			t.Errorf("view lacks graph line %q:\n%s", line, v)
		}
	}

	press(m, "down")
	v = screen(m)
	if !strings.Contains(v, "g2 has no tasks yet (the plan is draft)") {
		t.Errorf("after down, view lacks g2's empty graph:\n%s", v)
	}
}

func TestTickPicksUpNewGoals(t *testing.T) {
	t.Parallel()
	s := seed(t)
	m, _ := newModel(t, s)
	if _, err := s.CreateGoal(context.Background(), "Third thing", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen(m), "Third thing") {
		t.Fatal("goal shown before the tick")
	}
	m.Update(tickMsg(clock.Add(time.Second)))
	if !strings.Contains(screen(m), "Third thing") {
		t.Errorf("goal missing after the tick:\n%s", screen(m))
	}
}

func TestNewPlanStartsPlanWithText(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	press(m, "n")
	press(m, "add", " ", "q", "?") // q and ? are text while typing
	if !strings.Contains(screen(m), "new goal:") {
		t.Fatalf("no text input shown:\n%s", screen(m))
	}
	press(m, "enter")
	if got, want := lastStart(t, f), []string{"plan", "add q?"}; !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestEscCancelsNewPlan(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	press(m, "n", "x", "esc")
	if len(f.Calls()) != 0 {
		t.Errorf("calls = %v, want none", f.Calls())
	}
	if strings.Contains(screen(m), "new goal:") {
		t.Errorf("text input still shown:\n%s", screen(m))
	}
}

func TestApproveStartsPlanApprove(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"A", "enter"} {
		m, f := newModel(t, seed(t))
		press(m, "down", key)
		if got, want := lastStart(t, f), []string{"plan", "approve", "g2"}; !slices.Equal(got, want) {
			t.Errorf("%s: args = %q, want %q", key, got, want)
		}
	}
}

func TestRunStartsRunForGoal(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	press(m, "R")
	if got, want := lastStart(t, f), []string{"run", "g1"}; !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestKeepStartsWatchAndSecondPressInterrupts(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	f.Script(FakeResult{Running: true}, "keep", "g1", "--watch")
	press(m, "k")
	if got, want := lastStart(t, f), []string{"keep", "g1", "--watch"}; !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	press(m, "k")
	if n := len(f.Calls()); n != 1 {
		t.Errorf("second k started another command: %v", f.Calls())
	}
	if n := f.Procs()[0].Interrupts(); n != 1 {
		t.Errorf("interrupts = %d, want 1", n)
	}
	if v := screen(m); !strings.Contains(v, "sent interrupt to mill keep g1 --watch") {
		t.Errorf("output panel does not report the interrupt:\n%s", v)
	}
}

func TestPauseWithNothingRunning(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	press(m, "p")
	if v := screen(m); !strings.Contains(v, "nothing to pause") {
		t.Errorf("view lacks %q:\n%s", "nothing to pause", v)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("calls = %v, want none", f.Calls())
	}
}

func TestPauseInterruptsLatestRunningRun(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	f.Script(FakeResult{Running: true}, "run", "g1")
	press(m, "R")
	press(m, "p")
	if n := f.Procs()[0].Interrupts(); n != 1 {
		t.Errorf("interrupts = %d, want 1", n)
	}
	if v := screen(m); !strings.Contains(v, "pause: sent interrupt to mill run g1") {
		t.Errorf("output panel does not report the pause:\n%s", v)
	}
}

func TestPauseSkipsFinishedCommands(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	cmd := press(m, "R") // unscripted: exits 0 at once
	m.Update(cmd())
	press(m, "p")
	if v := screen(m); !strings.Contains(v, "nothing to pause") {
		t.Errorf("view lacks %q:\n%s", "nothing to pause", v)
	}
	if n := f.Procs()[0].Interrupts(); n != 0 {
		t.Errorf("interrupts = %d, want 0", n)
	}
}

func TestFailedCommandShowsExitCodeAndStderr(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	f.Script(FakeResult{Stderr: "mill run: goal g1 is blocked\n", Code: 1}, "run", "g1")
	cmd := press(m, "R")
	if cmd == nil {
		t.Fatal("no wait command returned")
	}
	m.Update(cmd())
	v := screen(m)
	if !strings.Contains(v, "mill run g1: failed with exit code 1") {
		t.Errorf("view lacks the exit code:\n%s", v)
	}
	if !strings.Contains(v, "mill run: goal g1 is blocked") {
		t.Errorf("view lacks stderr:\n%s", v)
	}
	if strings.Contains(v, "✓ mill run g1") {
		t.Errorf("failed command reported as success:\n%s", v)
	}
}

func TestSucceededCommandShowsExitZero(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	f.Script(FakeResult{Stdout: "g1 started\n"}, "run", "g1")
	m.Update(press(m, "R")())
	v := screen(m)
	if !strings.Contains(v, "✓ mill run g1: exit code 0") || !strings.Contains(v, "g1 started") {
		t.Errorf("view lacks success and output:\n%s", v)
	}
}

func TestCommandThatCannotStartIsReported(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seed(t))
	f.Script(FakeResult{StartErr: errors.New("no such file")}, "run", "g1")
	if cmd := press(m, "R"); cmd != nil {
		t.Error("a command that did not start returned a wait command")
	}
	if v := screen(m); !strings.Contains(v, "✗ mill run g1: could not start: no such file") {
		t.Errorf("view lacks the start failure:\n%s", v)
	}
}

func TestHelpListsKeysAndSkipNotAvailable(t *testing.T) {
	t.Parallel()
	m, _ := newModel(t, seed(t))
	press(m, "?")
	v := screen(m)
	for _, k := range []string{"a ", "c ", "x ", "r ", "l ", "p ", "o "} {
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(k)).MatchString(v) {
			t.Errorf("help lacks key %q:\n%s", k, v)
		}
	}
	if !strings.Contains(v, "s skip (not available: mill has no skip)") &&
		!regexp.MustCompile(`s\s+skip \(not available: mill has no skip\)`).MatchString(v) {
		t.Errorf("help does not list s as not available:\n%s", v)
	}
	press(m, "?")
	if strings.Contains(screen(m), "mill has no skip") {
		t.Error("? did not close the help")
	}
}

func TestQuitReturnsTeaQuit(t *testing.T) {
	t.Parallel()
	m, _ := newModel(t, seed(t))
	cmd := press(m, "q")
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q returned %T, want tea.QuitMsg", cmd())
	}
}

func TestTabsSwitchViews(t *testing.T) {
	t.Parallel()
	m, _ := newModel(t, seed(t))
	press(m, "tab")
	if v := screen(m); !strings.Contains(v, "no tasks await review") || strings.Contains(v, "Build the widget") {
		t.Errorf("tab did not switch to Review:\n%s", v)
	}
	press(m, "shift+tab")
	if v := screen(m); !strings.Contains(v, "Build the widget") {
		t.Errorf("shift+tab did not return to Goals:\n%s", v)
	}
	press(m, "shift+tab")
	if v := screen(m); !strings.Contains(v, "nothing running") {
		t.Errorf("shift+tab did not wrap to Agents:\n%s", v)
	}
}

// failingStore fails every ListGoals call.
type failingStore struct{ Store }

func (failingStore) ListGoals(context.Context) ([]store.Goal, error) {
	return nil, errors.New("database is locked")
}

func TestStoreErrorIsShownNotHidden(t *testing.T) {
	t.Parallel()
	m, _ := newModel(t, failingStore{seed(t)})
	if v := screen(m); !strings.Contains(v, "reading the store failed: database is locked") {
		t.Errorf("view hides the store error:\n%s", v)
	}
}

// TestPackageHasNoStoreWritesOrSQL checks the package source: the TUI only reads the store.
func TestPackageHasNoStoreWritesOrSQL(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writes := regexp.MustCompile(`\.(Create|Approve|Set|Update[A-Z]|Start[A-Z]|Finish|Append|Reject|Request|Reset|Save|Mark|Recover|Clear)\w*\(ctx`)
	sql := regexp.MustCompile(`(?i)\b(SELECT\s.+\sFROM|INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM)\b|database/sql|\.DB\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := writes.FindIndex(b); loc != nil {
			t.Errorf("%s calls a store write method: %s", f, b[loc[0]:loc[1]])
		}
		if loc := sql.FindIndex(b); loc != nil {
			t.Errorf("%s contains SQL: %s", f, b[loc[0]:loc[1]])
		}
	}
}
