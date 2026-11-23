package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/tui"
)

// tuiDeadline bounds every wait of the TUI end-to-end tests.
const tuiDeadline = 2 * time.Minute

// tuiTasks are the tasks of testdata/tui/plan.json: app depends on lib.
var tuiTasks = []string{"g1/lib", "g1/app"}

// inProcRunner is the TUI's CommandRunner for tests: it runs each mill command in-process
// through Run with the test's Env instead of spawning the mill binary.
type inProcRunner struct {
	env    Env
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup // every Capture and Start still running
}

func newInProcRunner(t *testing.T, env Env) *inProcRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &inProcRunner{env: env, ctx: ctx, cancel: cancel}
	// Stop and wait for started commands before the temp dirs are removed.
	t.Cleanup(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancel()
		r.wg.Wait()
	})
	return r
}

// begin registers a command; it fails once the test is cleaning up.
func (r *inProcRunner) begin(args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("mill %s: the test is over", strings.Join(args, " "))
	}
	r.wg.Add(1)
	return nil
}

func (r *inProcRunner) Capture(ctx context.Context, args []string) (string, string, int, error) {
	if err := r.begin(args); err != nil {
		return "", "", -1, err
	}
	defer r.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer context.AfterFunc(r.ctx, cancel)()
	var out, errb bytes.Buffer
	env := r.env
	env.Stdout, env.Stderr = &out, &errb
	code := Run(ctx, env, args)
	return out.String(), errb.String(), code, nil
}

func (r *inProcRunner) Start(args []string, out io.Writer) (tui.Proc, error) {
	if err := r.begin(args); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	p := &inProcProc{args: append([]string(nil), args...), cancel: cancel, done: make(chan struct{})}
	env := r.env
	env.Stdout, env.Stderr = out, out
	go func() {
		defer r.wg.Done()
		defer cancel()
		p.code = Run(ctx, env, args)
		close(p.done)
	}()
	return p, nil
}

// inProcProc is a command started by inProcRunner; Interrupt cancels its context.
type inProcProc struct {
	args   []string
	cancel context.CancelFunc
	done   chan struct{}
	code   int
}

func (p *inProcProc) Interrupt() error { p.cancel(); return nil }
func (p *inProcProc) Wait() (int, error) {
	<-p.done
	return p.code, nil
}
func (p *inProcProc) Args() []string { return p.args }

// tuiDriver owns a TUI model and plays the Bubble Tea loop: commands run in goroutines and
// their messages are fed back to Update on the test goroutine.
type tuiDriver struct {
	t    *testing.T
	m    *tui.Model
	msgs chan tea.Msg
	done chan struct{}
}

func newTUIDriver(t *testing.T, root string) *tuiDriver {
	t.Helper()
	env := Env{Dir: root, Home: t.TempDir()}
	d := &tuiDriver{t: t, msgs: make(chan tea.Msg, 64), done: make(chan struct{})}
	s := openStore(t, root)
	r := newInProcRunner(t, env)
	t.Cleanup(func() { close(d.done) })
	d.m = tui.New(tui.Options{Store: s, Paths: project.PathsAt(root), Runner: r})
	// Tall enough that the output panel (a quarter of the height) keeps every line.
	d.send(tea.WindowSizeMsg{Width: 400, Height: 800})
	d.exec(d.m.Init())
	return d
}

func (d *tuiDriver) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				d.exec(c)
			}
			return
		}
		if msg == nil {
			return
		}
		select {
		case d.msgs <- msg:
		case <-d.done:
		}
	}()
}

func (d *tuiDriver) send(msg tea.Msg) {
	_, cmd := d.m.Update(msg)
	d.exec(cmd)
}

// keys presses s as typed runes.
func (d *tuiDriver) keys(s string) {
	d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func (d *tuiDriver) press(k tea.KeyType) { d.send(tea.KeyMsg{Type: k}) }

// waitFor feeds messages to the model (the poll ticks among them) until cond holds for the
// screen, failing at the deadline.
func (d *tuiDriver) waitFor(what string, cond func(screen string) bool) string {
	d.t.Helper()
	deadline := time.NewTimer(tuiDeadline)
	defer deadline.Stop()
	for {
		if screen := d.m.View(); cond(screen) {
			return screen
		}
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-deadline.C:
			d.t.Fatalf("timed out waiting for %s; screen:\n%s", what, d.m.View())
		}
	}
}

// waitCommand waits until `mill cmd` started by the TUI exits and returns the screen.
func (d *tuiDriver) waitCommand(cmd string) string {
	d.t.Helper()
	return d.waitFor("mill "+cmd+" to exit", func(s string) bool {
		return strings.Contains(s, "✓ mill "+cmd+":") || strings.Contains(s, "✗ mill "+cmd+":")
	})
}

// waitSuccess waits until `mill cmd` exits and fails the test unless it exited 0.
func (d *tuiDriver) waitSuccess(cmd string) string {
	d.t.Helper()
	s := d.waitCommand(cmd)
	if !strings.Contains(s, "✓ mill "+cmd+": exit code 0") {
		d.t.Fatalf("mill %s did not succeed:\n%s", cmd, s)
	}
	return s
}

// tuiRepo makes an initialized repo on main with a bare origin, the fake runner (planner and
// workers scripted for testdata/tui/plan.json) and host.kind set to hostKind ("" leaves it unset).
func tuiRepo(t *testing.T, hostKind string) string {
	t.Helper()
	plan, err := os.ReadFile(filepath.Join("testdata", "tui", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := gitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, root, "checkout", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, root, nil, "init"); r.code != ExitOK {
		t.Fatalf("init: %+v", r)
	}
	cfg := `check_cmd = "true"
max_attempts = 1
max_parallel = 2
worktree_root = "` + t.TempDir() + `"
[runner]
kind = "fake"
fake_script = "script.json"
[budget.task]
usd = 0.0
`
	if hostKind != "" {
		cfg += "[host]\nkind = \"" + hostKind + "\"\n"
	}
	if err := os.WriteFile(project.PathsAt(root).Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `{"planner": [{"output": ` + string(plan) + `}],
"tasks": {
  "g1/lib": ` + taskSteps(`{"lib.txt": "hello\n"}`) + `,
  "g1/app": ` + taskSteps(`{"app.txt": "app\n"}`) + `
}}`
	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.CommitAll(ctx, root, "setup"); err != nil {
		t.Fatal(err)
	}
	addRemote(t, root)
	return root
}

// planAndApproveViaTUI plans g1 with the fake planner (n, typed goal, enter) and approves
// the plan (A), all through key presses.
func planAndApproveViaTUI(t *testing.T, d *tuiDriver) {
	t.Helper()
	d.keys("n")
	d.keys("greet the user")
	d.press(tea.KeyEnter)
	d.waitSuccess("plan greet the user")
	d.waitFor("goal g1 in the Goals view", func(s string) bool { return strings.Contains(s, "> g1") })
	d.keys("A")
	d.waitSuccess("plan approve g1")
	d.waitFor("the approved goal's tasks", func(s string) bool {
		return strings.Contains(s, "> g1    approved")
	})
}

// runGoalViaTUI runs g1 with R and polls until every task is verified.
func runGoalViaTUI(t *testing.T, d *tuiDriver, root string) {
	t.Helper()
	d.keys("R")
	s := openStore(t, root)
	d.waitFor("every task of g1 to be verified", func(string) bool {
		for _, id := range tuiTasks {
			tk, err := s.GetTask(context.Background(), id)
			if err != nil || tk.Status != store.TaskVerified {
				return false
			}
		}
		return true
	})
	d.waitSuccess("run g1")
}

// goalResults describes what the run of g1 produced: each task's status, attempts and the
// diff of its branch against main.
func goalResults(t *testing.T, root string) map[string]string {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, root)
	res := map[string]string{}
	for _, id := range tuiTasks {
		tk, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		atts, err := s.Attempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		diff, err := gitx.Run(ctx, root, "diff", "main", "mill/"+id)
		if err != nil {
			t.Fatal(err)
		}
		res[id] = fmt.Sprintf("%s, %d attempt(s)\n%s", tk.Status, len(atts), diff)
	}
	return res
}

func TestTUIPlanRunReviewApproveAndRejectWithExecHost(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	root := tuiRepo(t, "exec")
	d := newTUIDriver(t, root)
	ctx := context.Background()

	planAndApproveViaTUI(t, d)
	runGoalViaTUI(t, d, root)

	// Review lists dependencies first: lib before app, though app comes first in the plan.
	d.press(tea.KeyTab)
	screen := d.waitFor("both tasks in Review", func(s string) bool {
		return strings.Contains(s, "> g1/lib") && strings.Contains(s, "g1/app")
	})
	if lib, app := strings.Index(screen, "g1/lib "), strings.Index(screen, "g1/app "); lib < 0 || app < 0 || lib > app {
		t.Fatalf("Review does not list g1/lib before g1/app:\n%s", screen)
	}

	// The detail shows the spec, the gate and the verdict.
	d.press(tea.KeyEnter)
	screen = d.waitFor("the detail of g1/lib", func(s string) bool { return strings.Contains(s, "last gate:") })
	for _, want := range []string{"g1/lib  verified  Lib", "write lib.txt with the greeting", "- lib.txt exists",
		"last verdict: pass: ok", "last gate: passed", "✓ test -f lib.txt (exit 0)"} {
		if !strings.Contains(screen, want) {
			t.Errorf("detail lacks %q:\n%s", want, screen)
		}
	}

	// a approves lib, which publishes it.
	d.keys("a")
	d.waitSuccess("review approve g1/lib")
	s := openStore(t, root)
	lib, err := s.GetTask(ctx, "g1/lib")
	if err != nil {
		t.Fatal(err)
	}
	// An approval with nothing to wait for publishes at once: the task is published, its
	// branch row records the pushed commit and the publish event is logged.
	if lib.Status != store.TaskPublished {
		t.Fatalf("g1/lib = %s, want published by the approval", lib.Status)
	}
	if br, err := s.GetBranch(ctx, "g1/lib"); err != nil || br.PublishedSHA == "" || br.PublishedRef == "" {
		t.Fatalf("g1/lib branch = %+v, %v, want the published commit recorded", br, err)
	}
	lastEvent(t, s, "g1/lib", "task-published")

	// x with a reason rejects app.
	d.press(tea.KeyEsc)
	d.waitFor("Review with g1/lib published", func(s string) bool { return strings.Contains(s, "> g1/lib") && strings.Contains(s, "published") })
	d.press(tea.KeyDown)
	d.waitFor("g1/app selected", func(s string) bool { return strings.Contains(s, "> g1/app") })
	d.keys("x")
	d.keys("wrong greeting")
	d.press(tea.KeyEnter)
	d.waitSuccess("review reject g1/app -m wrong greeting")
	app, err := s.GetTask(ctx, "g1/app")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != store.TaskBlocked || app.Reason != "rejected: wrong greeting" || app.Approved {
		t.Fatalf("g1/app = %s (%s) approved=%v, want blocked as rejected", app.Status, app.Reason, app.Approved)
	}
}

// fakeHerdrOnPath puts testdata/tui/herdr first on PATH and returns its log file. It is the
// only fake herdr of package cli; callers set HERDR_ENV (and HERDR_WORKSPACE_ID) themselves.
func fakeHerdrOnPath(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "tui", "herdr"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "herdr"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "herdr.log")
}

func TestTUIRunsGoalInHerdrTabsLikeExecHost(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	execRoot := tuiRepo(t, "exec")
	d := newTUIDriver(t, execRoot)
	planAndApproveViaTUI(t, d)
	runGoalViaTUI(t, d, execRoot)
	want := goalResults(t, execRoot)

	log := fakeHerdrOnPath(t)
	t.Setenv("HERDR_ENV", "1")
	root := tuiRepo(t, "auto")
	d = newTUIDriver(t, root)
	planAndApproveViaTUI(t, d)
	runGoalViaTUI(t, d, root)

	got := goalResults(t, root)
	for _, id := range tuiTasks {
		if got[id] != want[id] {
			t.Errorf("%s under herdr:\n%s\nunder exec:\n%s", id, got[id], want[id])
		}
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the fake herdr was never called: %v", err)
	}
	counts := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || (f[0] != "tab" && f[0] != "pane") {
			t.Fatalf("unexpected herdr call %q in:\n%s", l, b)
		}
		if f[0] == "tab" && f[1] == "create" && (!strings.Contains(l, " --label mill ") || strings.Contains(l, "--focus")) {
			t.Fatalf("tab create %q lacks the mill label or asks for focus", l)
		}
		counts[f[0]+" "+f[1]]++
	}
	// Each task's gate runs check_cmd and the task's check, each in its own tab.
	created := counts["tab create"]
	if created < 2*len(tuiTasks) || counts["pane run"] != created || counts["tab close"] != created || len(counts) != 3 {
		t.Fatalf("herdr calls = %v, want a tab create, pane run and tab close per worker process:\n%s", counts, b)
	}
}

func TestTUIRunWithHerdrHostOutsideHerdrShowsError(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	root := tuiRepo(t, "")
	d := newTUIDriver(t, root)
	planAndApproveViaTUI(t, d)
	// Planning needs a host too, so host.kind = herdr is set only before the run.
	useHostKind(t, root, "herdr")

	d.keys("R")
	screen := d.waitCommand("run g1")
	for _, want := range []string{"✗ mill run g1: failed with exit code 1",
		"mill run: host.kind = herdr needs mill to run inside a herdr pane (HERDR_ENV=1) with herdr on PATH"} {
		if !strings.Contains(screen, want) {
			t.Errorf("output panel lacks %q:\n%s", want, screen)
		}
	}
	tk, err := openStore(t, root).GetTask(context.Background(), "g1/lib")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskPending {
		t.Errorf("g1/lib = %s after the refused run, want pending", tk.Status)
	}
}
