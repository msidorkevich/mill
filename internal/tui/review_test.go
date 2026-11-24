package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// seedReview opens a store with g1 (approved: z, m <- z, both verified, so the store lists m
// first) and the standalone task solo (verified).
func seedReview(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "mill.db"), store.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g, err := s.CreateGoal(ctx, "Ship it", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Plan{Goal: "Ship it", Tasks: []taskspec.Spec{spec("z"), spec("m", "z")}}
	if err := s.ApproveGoal(ctx, g.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, spec("solo")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1/z", "g1/m", "solo"} {
		if err := s.SetTaskStatus(ctx, id, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// newModelWith builds a model like newModel, with env as the environment and edit as the editor.
func newModelWith(t *testing.T, s Store, env map[string]string, edit EditFunc) (*Model, *FakeRunner) {
	t.Helper()
	f := NewFakeRunner()
	m := New(Options{
		Store:    s,
		Paths:    project.PathsAt(t.TempDir()),
		Runner:   f,
		Now:      func() time.Time { return clock },
		Getenv:   func(k string) string { return env[k] },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Edit:     edit,
		TempDir:  t.TempDir(),
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m.Update(tickMsg(clock))
	return m, f
}

// drain runs cmd and feeds the messages of this package's own commands back to the model,
// following the commands they return. Other messages (cursor blinks, ticks) are dropped.
func drain(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drain(m, c)
		}
	case procDoneMsg, openDetailMsg, backToReviewMsg, detailMsg, editedMsg, retryReadyMsg:
		_, next := m.Update(msg)
		drain(m, next)
	}
}

// toReview switches from Goals to the Review tab.
func toReview(m *Model) { press(m, "tab") }

// lastStarted returns the args of the last Start, ignoring the captures that refresh the
// detail view after a command finishes.
func lastStarted(t *testing.T, f *FakeRunner) []string {
	t.Helper()
	s := starts(f)
	if len(s) == 0 {
		t.Fatalf("nothing was started; calls = %v", f.Calls())
	}
	return s[len(s)-1]
}

func starts(f *FakeRunner) [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if c.Method == "Start" {
			out = append(out, c.Args)
		}
	}
	return out
}

func TestReviewListsTasksDependenciesFirst(t *testing.T) {
	t.Parallel()
	s := seedReview(t)
	if err := s.ApproveTask(context.Background(), "solo", 0); err != nil {
		t.Fatal(err)
	}
	m, _ := newModel(t, s)
	toReview(m)
	v := screen(m)
	z, mm, solo := strings.Index(v, "g1/z "), strings.Index(v, "g1/m "), strings.Index(v, "solo ")
	if z < 0 || mm < 0 || solo < 0 {
		t.Fatalf("view lacks a review task:\n%s", v)
	}
	if z > mm {
		t.Errorf("g1/m is listed before its dependency g1/z:\n%s", v)
	}
	for _, want := range []string{"verified", "Z", "M", "SOLO", "approved at attempt 0", "not approved"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "> g1/z") {
		t.Errorf("the first task is not selected:\n%s", v)
	}
}

func TestDependencyOrderKeepsStoreOrderOtherwise(t *testing.T) {
	t.Parallel()
	review := []store.ReviewTask{{TaskSummary: store.TaskSummary{ID: "g1/c"}}, {TaskSummary: store.TaskSummary{ID: "g1/a"}},
		{TaskSummary: store.TaskSummary{ID: "x"}}, {TaskSummary: store.TaskSummary{ID: "g1/b"}}}
	tasks := map[string][]goalTask{"g1": {
		{Task: store.Task{Spec: taskspec.Spec{ID: "g1/a"}}},
		{Task: store.Task{Spec: taskspec.Spec{ID: "g1/mid"}}, Deps: []string{"g1/b"}}, // landed, not in the queue
		{Task: store.Task{Spec: taskspec.Spec{ID: "g1/b"}}},
		{Task: store.Task{Spec: taskspec.Spec{ID: "g1/c"}}, Deps: []string{"g1/mid"}},
	}}
	var got []string
	for _, t := range dependencyOrder(review, tasks) {
		got = append(got, t.ID)
	}
	if want := []string{"g1/a", "x", "g1/b", "g1/c"}; !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestReviewEnterOpensTaskDetail(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seedReview(t))
	toReview(m)
	press(m, "down")
	drain(m, press(m, "enter"))
	if m.active != viewDetail {
		t.Fatalf("enter did not switch to Task detail (active = %d)", m.active)
	}
	var captured []string
	for _, c := range f.Calls() {
		captured = append(captured, strings.Join(c.Args, " "))
	}
	want := []string{"task show g1/m --json", "review show g1/m --json", "review show g1/m --diff"}
	if !slices.Equal(captured, want) {
		t.Errorf("captures = %q, want %q", captured, want)
	}
	drain(m, press(m, "esc"))
	if m.active != viewReview {
		t.Errorf("esc did not return to Review (active = %d)", m.active)
	}
}

// inBothViews runs fn once with the Review view and once with the Task detail view showing
// the task at row (0: g1/z, 1: g1/m, 2: solo).
func inBothViews(t *testing.T, row int, newM func(t *testing.T) (*Model, *FakeRunner), fn func(t *testing.T, m *Model, f *FakeRunner)) {
	for _, name := range []string{"review", "detail"} {
		t.Run(name, func(t *testing.T) {
			m, f := newM(t)
			toReview(m)
			for range row {
				press(m, "down")
			}
			if name == "detail" {
				drain(m, press(m, "enter"))
				if m.active != viewDetail {
					t.Fatal("enter did not open Task detail")
				}
			}
			fn(t, m, f)
		})
	}
}

func plainModel(t *testing.T) (*Model, *FakeRunner) { return newModel(t, seedReview(t)) }

func TestApproveKeyStartsReviewApprove(t *testing.T) {
	t.Parallel()
	inBothViews(t, 1, plainModel, func(t *testing.T, m *Model, f *FakeRunner) {
		press(m, "a")
		if got, want := lastStart(t, f), []string{"review", "approve", "g1/m"}; !slices.Equal(got, want) {
			t.Errorf("args = %q, want %q", got, want)
		}
	})
}

func TestRejectStartsReviewRejectWithReason(t *testing.T) {
	t.Parallel()
	inBothViews(t, 0, plainModel, func(t *testing.T, m *Model, f *FakeRunner) {
		press(m, "x")
		if !strings.Contains(screen(m), "reject reason:") {
			t.Fatalf("no reason input shown:\n%s", screen(m))
		}
		press(m, "too", " ", "risky", " ", "q") // q is text while typing
		press(m, "enter")
		if got, want := lastStart(t, f), []string{"review", "reject", "g1/z", "-m", "too risky q"}; !slices.Equal(got, want) {
			t.Errorf("args = %q, want %q", got, want)
		}
		if strings.Contains(screen(m), "reject reason:") {
			t.Errorf("input still shown after enter:\n%s", screen(m))
		}
	})
}

func TestRejectWithEmptyReasonRunsNothing(t *testing.T) {
	t.Parallel()
	inBothViews(t, 0, plainModel, func(t *testing.T, m *Model, f *FakeRunner) {
		press(m, "x", " ", "enter")
		if s := starts(f); len(s) != 0 {
			t.Errorf("starts = %q, want none", s)
		}
		if v := screen(m); !strings.Contains(v, "reject: type the reason first") || !strings.Contains(v, "reject reason:") {
			t.Errorf("empty reason not refused:\n%s", v)
		}
		press(m, "esc")
		if v := screen(m); strings.Contains(v, "reject reason:") {
			t.Errorf("esc did not cancel the input:\n%s", v)
		}
		if s := starts(f); len(s) != 0 {
			t.Errorf("starts after esc = %q, want none", s)
		}
	})
}

// stubEditor appends text to the file it edits, or fails with err.
func stubEditor(text string, err error, edited *[]string) EditFunc {
	return func(editor, path string, done func(error) tea.Msg) tea.Cmd {
		*edited = append(*edited, editor+" "+path)
		return func() tea.Msg {
			if err != nil {
				return done(err)
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return done(rerr)
			}
			return done(os.WriteFile(path, append([]byte(text), b...), 0o600))
		}
	}
}

func TestChangesEditsCommentsThenStartsReviewChanges(t *testing.T) {
	t.Parallel()
	var edited []string
	newM := func(t *testing.T) (*Model, *FakeRunner) {
		return newModelWith(t, seedReview(t), map[string]string{"EDITOR": "myeditor -w"}, stubEditor("Fix the nil check.\n\nAdd a test.\n", nil, &edited))
	}
	inBothViews(t, 1, newM, func(t *testing.T, m *Model, f *FakeRunner) {
		edited = nil
		drain(m, press(m, "c"))
		if len(edited) != 1 || !strings.HasPrefix(edited[0], "myeditor -w ") {
			t.Fatalf("editor runs = %q, want one run of $EDITOR", edited)
		}
		args := lastStarted(t, f)
		if len(args) != 5 || !slices.Equal(args[:4], []string{"review", "changes", "g1/m", "--file"}) {
			t.Fatalf("args = %q, want [review changes g1/m --file <path>]", args)
		}
		b, err := os.ReadFile(args[4])
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(b)); got != "Fix the nil check.\n\nAdd a test." {
			t.Errorf("comment file = %q, want the edited text without the template's # lines", got)
		}
	})
}

func TestChangesWritesCommentFileInTempDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	var edited []string
	m := New(Options{
		Store:    seedReview(t),
		Paths:    project.PathsAt(t.TempDir()),
		Runner:   NewFakeRunner(),
		Now:      func() time.Time { return clock },
		Getenv:   func(k string) string { return map[string]string{"EDITOR": "vi"}[k] },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Edit:     stubEditor("Fix it.\n", nil, &edited),
		TempDir:  tmp,
	})
	f := m.opts.Runner.(*FakeRunner)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m.Update(tickMsg(clock))
	toReview(m)
	drain(m, press(m, "c"))
	args := lastStarted(t, f)
	if path := args[len(args)-1]; filepath.Dir(path) != tmp {
		t.Fatalf("comment file = %q, want it in %q", path, tmp)
	}
}

func TestChangesWithFailingEditorRunsNothing(t *testing.T) {
	t.Parallel()
	var edited []string
	newM := func(t *testing.T) (*Model, *FakeRunner) {
		return newModelWith(t, seedReview(t), map[string]string{"EDITOR": "vi"}, stubEditor("", errors.New("exit status 1"), &edited))
	}
	inBothViews(t, 0, newM, func(t *testing.T, m *Model, f *FakeRunner) {
		drain(m, press(m, "c"))
		if s := starts(f); len(s) != 0 {
			t.Errorf("starts = %q, want none", s)
		}
		if v := screen(m); !strings.Contains(v, "✗ changes: the editor failed: exit status 1 (nothing was run)") {
			t.Errorf("view lacks the editor failure:\n%s", v)
		}
	})
}

func TestChangesWithoutEditorRunsNothing(t *testing.T) {
	t.Parallel()
	var edited []string
	m, f := newModelWith(t, seedReview(t), nil, stubEditor("x", nil, &edited))
	toReview(m)
	drain(m, press(m, "c"))
	if len(edited) != 0 || len(starts(f)) != 0 {
		t.Errorf("editor runs = %q, starts = %q; want none", edited, starts(f))
	}
	if v := screen(m); !strings.Contains(v, "$EDITOR is not set") {
		t.Errorf("view does not report the missing $EDITOR:\n%s", v)
	}
}

func TestRetryGoalTaskStartsRunRetryBlocked(t *testing.T) {
	t.Parallel()
	inBothViews(t, 1, plainModel, func(t *testing.T, m *Model, f *FakeRunner) {
		press(m, "r")
		if got, want := lastStart(t, f), []string{"run", "g1", "--retry-blocked"}; !slices.Equal(got, want) {
			t.Errorf("args = %q, want %q", got, want)
		}
	})
}

func TestRetryStandaloneTaskRunsItsSpecWithRetry(t *testing.T) {
	t.Parallel()
	inBothViews(t, 2, plainModel, func(t *testing.T, m *Model, f *FakeRunner) {
		f.Script(FakeResult{Stdout: taskShowJSON(t, spec("solo"), 1, 0.5)}, "task", "show", "solo", "--json")
		drain(m, press(m, "r"))
		args := lastStarted(t, f)
		if len(args) != 5 || !slices.Equal(args[:3], []string{"task", "run", "--file"}) || args[4] != "--retry" {
			t.Fatalf("args = %q, want [task run --file <tmp> --retry]", args)
		}
		got, err := taskspec.Load(args[3])
		if err != nil {
			t.Fatalf("the spec file does not load: %v", err)
		}
		if want := spec("solo"); got.ID != want.ID || got.Title != want.Title || !slices.Equal(got.Checks, want.Checks) {
			t.Errorf("spec file = %+v, want %+v", got, want)
		}
	})
}

func TestRetryStandaloneTaskWithFailedShowRunsNothing(t *testing.T) {
	t.Parallel()
	m, f := newModel(t, seedReview(t))
	f.Script(FakeResult{Stderr: "mill task show: database is locked\n", Code: 1}, "task", "show", "solo", "--json")
	toReview(m)
	press(m, "down", "down")
	drain(m, press(m, "r"))
	if s := starts(f); len(s) != 0 {
		t.Errorf("starts = %q, want none", s)
	}
	if v := screen(m); !strings.Contains(v, "mill task show solo --json failed with exit code 1: mill task show: database is locked") {
		t.Errorf("view lacks the CLI's stderr:\n%s", v)
	}
}
