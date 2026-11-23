package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// openDetailMsg asks the model to show the Task detail view for a task.
type openDetailMsg struct{ task string }

// retryReadyMsg reports that a standalone task's spec was written for `task run --retry`.
type retryReadyMsg struct {
	task string
	path string
	err  string // why the spec could not be written; nothing runs then
}

// taskActions are the a/c/x/r keys on a task, shared by the Review and Task detail views.
type taskActions struct {
	cmds      *commands
	getenv    func(string) string
	edit      EditFunc
	input     textinput.Model
	rejecting string // the task whose reject reason is being typed
}

func newTaskActions(cmds *commands, getenv func(string) string, edit EditFunc) *taskActions {
	in := textinput.New()
	in.Placeholder = "why is it rejected?"
	in.Prompt = "reject reason: "
	return &taskActions{cmds: cmds, getenv: getenv, edit: edit, input: in}
}

func (a *taskActions) capturing() bool { return a.rejecting != "" }

// key handles a/c/x/r for task id; ok is false for other keys.
func (a *taskActions) key(id string, msg tea.KeyMsg) (cmd tea.Cmd, ok bool) {
	verb := map[string]string{"a": "approve", "c": "changes", "x": "reject", "r": "retry"}[msg.String()]
	if verb == "" {
		return nil, false
	}
	if id == "" {
		a.cmds.note("%s: no task selected", verb)
		return nil, true
	}
	switch verb {
	case "approve":
		return a.cmds.start("review", "approve", id), true
	case "changes":
		return a.changes(id), true
	case "reject":
		a.rejecting = id
		return a.input.Focus(), true
	default:
		return a.retry(id), true
	}
}

// changes writes the comment template and opens $EDITOR on it.
func (a *taskActions) changes(id string) tea.Cmd {
	editor := a.getenv("EDITOR")
	if strings.TrimSpace(editor) == "" {
		a.cmds.note("✗ changes: $EDITOR is not set; set it to your editor and press c again (nothing was run)")
		return nil
	}
	f, err := os.CreateTemp("", "mill-changes-*.md")
	if err != nil {
		a.cmds.note("✗ changes: could not create the comment file: %v", err)
		return nil
	}
	_, werr := fmt.Fprintf(f, changesTemplate, id)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		a.cmds.note("✗ changes: could not write the comment file %s: %v", f.Name(), errors.Join(werr, cerr))
		return nil
	}
	path := f.Name()
	return a.edit(editor, path, func(err error) tea.Msg { return editedMsg{task: id, path: path, err: err} })
}

// retry starts the retry of a goal task, or prepares the spec of a standalone task.
func (a *taskActions) retry(id string) tea.Cmd {
	if goal, _ := store.SplitTaskID(id); goal != "" {
		return a.cmds.start("run", goal, "--retry-blocked")
	}
	runner := a.cmds.runner
	return func() tea.Msg {
		args := []string{"task", "show", id, "--json"}
		stdout, stderr, code, err := runner.Capture(context.Background(), args)
		if msg := captureFailure(args, stderr, code, err); msg != "" {
			return retryReadyMsg{task: id, err: msg}
		}
		var shown struct {
			Task taskspec.Spec `json:"task"`
		}
		if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
			return retryReadyMsg{task: id, err: fmt.Sprintf("mill task show %s --json printed invalid JSON: %v", id, err)}
		}
		b, err := json.MarshalIndent(shown.Task, "", "  ")
		if err != nil {
			return retryReadyMsg{task: id, err: fmt.Sprintf("encode the spec of %s: %v", id, err)}
		}
		f, err := os.CreateTemp("", "mill-retry-*.json")
		if err != nil {
			return retryReadyMsg{task: id, err: fmt.Sprintf("create the spec file: %v", err)}
		}
		_, werr := f.Write(append(b, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return retryReadyMsg{task: id, err: fmt.Sprintf("write the spec file %s: %v", f.Name(), werr)}
		}
		return retryReadyMsg{task: id, path: f.Name()}
	}
}

// captureFailure describes a failed Capture with the CLI's stderr, or returns "" on success.
func captureFailure(args []string, stderr string, code int, err error) string {
	cmd := "mill " + strings.Join(args, " ")
	switch {
	case err != nil:
		return fmt.Sprintf("%s could not run: %v", cmd, err)
	case code != 0:
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "(no stderr)"
		}
		return fmt.Sprintf("%s failed with exit code %d: %s", cmd, code, msg)
	}
	return ""
}

// updateInput handles keys while the reject reason is typed.
func (a *taskActions) updateInput(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		a.closeInput()
		return nil
	case tea.KeyEnter:
		reason := strings.TrimSpace(a.input.Value())
		if reason == "" {
			a.cmds.note("reject: type the reason first (esc cancels)")
			return nil
		}
		id := a.rejecting
		a.closeInput()
		return a.cmds.start("review", "reject", id, "-m", reason)
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(msg)
	return cmd
}

func (a *taskActions) closeInput() {
	a.rejecting = ""
	a.input.Blur()
	a.input.Reset()
}

// handle processes the messages the actions' own commands produce; ok is false for others.
func (a *taskActions) handle(msg tea.Msg) (cmd tea.Cmd, ok bool) {
	switch msg := msg.(type) {
	case editedMsg:
		if msg.err != nil {
			a.cmds.note("✗ changes: the editor failed: %v (nothing was run)", msg.err)
			return nil, true
		}
		left, err := stripComments(msg.path)
		if err != nil {
			a.cmds.note("✗ changes: could not read the comment file %s: %v", msg.path, err)
			return nil, true
		}
		if !left {
			a.cmds.note("changes: no comments written; nothing was run")
			return nil, true
		}
		return a.cmds.start("review", "changes", msg.task, "--file", msg.path), true
	case retryReadyMsg:
		if msg.err != "" {
			a.cmds.note("✗ retry %s: %s", msg.task, msg.err)
			return nil, true
		}
		return a.cmds.start("task", "run", "--file", msg.path, "--retry"), true
	}
	if a.capturing() {
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(msg)
		return cmd, true
	}
	return nil, false
}

// inputView renders the reject reason input while it is open.
func (a *taskActions) inputView() string {
	if !a.capturing() {
		return ""
	}
	return fmt.Sprintf("reject %s\n%s\n", a.rejecting, a.input.View())
}

// reviewView lists the tasks awaiting review, dependencies first.
type reviewView struct {
	acts   *taskActions
	snap   snapshot
	tasks  []store.ReviewTask
	cursor int
}

func newReviewView(acts *taskActions) *reviewView { return &reviewView{acts: acts} }

func (v *reviewView) CapturingInput() bool { return v.acts.capturing() }

func (v *reviewView) SelectedTask() string {
	if v.cursor < 0 || v.cursor >= len(v.tasks) {
		return ""
	}
	return v.tasks[v.cursor].ID
}

func (v *reviewView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case snapshot:
		sel := v.SelectedTask()
		v.snap = msg
		v.tasks = dependencyOrder(msg.Review, msg.Tasks)
		v.cursor = min(max(v.cursor, 0), max(len(v.tasks)-1, 0))
		for i, t := range v.tasks {
			if t.ID == sel {
				v.cursor = i
			}
		}
		return nil
	case tea.KeyMsg:
		if v.acts.capturing() {
			return v.acts.updateInput(msg)
		}
		if cmd, ok := v.acts.key(v.SelectedTask(), msg); ok {
			return cmd
		}
		switch msg.String() {
		case "up":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down":
			if v.cursor < len(v.tasks)-1 {
				v.cursor++
			}
		case "enter":
			id := v.SelectedTask()
			if id == "" {
				v.acts.cmds.note("detail: no task selected")
				return nil
			}
			return func() tea.Msg { return openDetailMsg{task: id} }
		}
		return nil
	}
	cmd, _ := v.acts.handle(msg)
	return cmd
}

// dependencyOrder orders review tasks so a task's dependencies come before it, keeping the
// store's order otherwise. deps come from the goals' task lists.
func dependencyOrder(review []store.ReviewTask, tasks map[string][]goalTask) []store.ReviewTask {
	deps := map[string][]string{}
	for _, ts := range tasks {
		for _, t := range ts {
			deps[t.ID] = t.Deps
		}
	}
	waiting := map[string]bool{}
	for _, t := range review {
		waiting[t.ID] = true
	}
	// blocked reports whether id waits on a review task not placed yet (transitively through
	// tasks outside the queue too).
	var blocked func(id string, seen map[string]bool) bool
	blocked = func(id string, seen map[string]bool) bool {
		for _, d := range deps[id] {
			if seen[d] {
				continue
			}
			seen[d] = true
			if waiting[d] || blocked(d, seen) {
				return true
			}
		}
		return false
	}
	out := make([]store.ReviewTask, 0, len(review))
	rest := append([]store.ReviewTask(nil), review...)
	for len(rest) > 0 {
		pick := 0 // on a cycle, fall back to the store's order
		for i, t := range rest {
			if !blocked(t.ID, map[string]bool{t.ID: true}) {
				pick = i
				break
			}
		}
		out = append(out, rest[pick])
		delete(waiting, rest[pick].ID)
		rest = append(rest[:pick], rest[pick+1:]...)
	}
	return out
}

func (v *reviewView) View(width, height int) string {
	var b strings.Builder
	if v.snap.Err != nil {
		fmt.Fprintf(&b, "✗ reading the store failed: %v\n", v.snap.Err)
	}
	b.WriteString(v.acts.inputView())
	if len(v.tasks) == 0 {
		b.WriteString("no tasks await review\n")
		return clip(b.String(), width, height)
	}
	for i, t := range v.tasks {
		mark := "  "
		if i == v.cursor {
			mark = "> "
		}
		approval := "not approved"
		if t.Approved {
			approval = fmt.Sprintf("approved at attempt %d", t.ApprovedAttempt)
		}
		fmt.Fprintf(&b, "%s%-24s %-9s %-40s %s\n", mark, t.ID, t.Status, t.Title, approval)
	}
	b.WriteString("\nenter detail  a approve  c changes  x reject  r retry\n")
	return clip(b.String(), width, height)
}
