package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/store"
)

type fixture struct {
	dir    string
	root   string
	st     *store.Store
	stderr *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "mill.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{dir: dir, root: root, st: st, stderr: &bytes.Buffer{}}
}

// runner returns a Runner whose clock advances by step on every call.
func (f *fixture) runner(cfg config.Config, step time.Duration) Runner {
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	return Runner{
		Store:  f.st,
		Cfg:    cfg,
		Root:   f.root,
		Stderr: f.stderr,
		Now: func() time.Time {
			t := at
			at = at.Add(step)
			return t
		},
	}
}

func (f *fixture) path(name string) string { return filepath.Join(f.dir, name) }

func hookConfig() config.Config {
	cfg := config.Default()
	cfg.HookTimeout = config.Duration{Duration: 10 * time.Second}
	return cfg
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hookEvents(t *testing.T, st *store.Store) []store.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.Kind == RunEvent {
			out = append(out, e)
		}
	}
	return out
}

type runData struct {
	Event      string `json:"event"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	OutputTail string `json:"output_tail"`
	TimedOut   bool   `json:"timed_out"`
}

func onlyEvent(t *testing.T, st *store.Store) (store.Event, runData) {
	t.Helper()
	evs := hookEvents(t, st)
	if len(evs) != 1 {
		t.Fatalf("got %d %s events, want 1", len(evs), RunEvent)
	}
	var d runData
	if err := json.Unmarshal(evs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	return evs[0], d
}

func TestRunPassesDocumentOnStdinFromRepoRoot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "cat > '" + f.path("stdin") + "'; pwd > '" + f.path("pwd") + "'; echo \"$MILL_HOOK_EVENT\" > '" + f.path("event") + "'"
	doc := map[string]string{"task": "g1/a", "branch": "mill/g1/a"}

	res, err := f.runner(cfg, 250*time.Millisecond).Run(context.Background(), "publish", "g1", "g1/a", doc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("result = %+v, want a successful run", res)
	}
	if res.Duration != 250*time.Millisecond {
		t.Errorf("duration = %s, want 250ms from the injected clock", res.Duration)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(read(t, f.path("stdin"))), &got); err != nil {
		t.Fatalf("stdin is not JSON: %v", err)
	}
	if got["task"] != "g1/a" || got["branch"] != "mill/g1/a" {
		t.Errorf("stdin document = %v", got)
	}
	wantRoot, _ := filepath.EvalSymlinks(f.root)
	gotRoot, _ := filepath.EvalSymlinks(strings.TrimSpace(read(t, f.path("pwd"))))
	if gotRoot != wantRoot {
		t.Errorf("hook ran in %q, want %q", gotRoot, wantRoot)
	}
	if ev := strings.TrimSpace(read(t, f.path("event"))); ev != "publish" {
		t.Errorf("MILL_HOOK_EVENT = %q, want publish", ev)
	}
	if f.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing for a successful hook", f.stderr.String())
	}
	ev, d := onlyEvent(t, f.st)
	if ev.GoalID != "g1" || ev.TaskID != "g1/a" {
		t.Errorf("event goal/task = %q/%q", ev.GoalID, ev.TaskID)
	}
	if d.Event != "publish" || d.Command != cfg.OnPublish || d.ExitCode != 0 || d.DurationMS != 250 || d.TimedOut {
		t.Errorf("event data = %+v", d)
	}
}

func TestRunUsesOnUpdateForUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "touch '" + f.path("publish") + "'"
	cfg.OnUpdate = "touch '" + f.path("update") + "'"
	if _, err := f.runner(cfg, 0).Run(context.Background(), "update", "g1", "g1/a", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("update")); err != nil {
		t.Errorf("on_update did not run: %v", err)
	}
	if _, err := os.Stat(f.path("publish")); err == nil {
		t.Error("on_publish ran for an update")
	}
}

func TestRunUnconfiguredHookIsSkippedWithoutEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnUpdate = "touch '" + f.path("update") + "'"

	res, err := f.runner(cfg, time.Second).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Errorf("result = %+v, want Skipped", res)
	}
	if evs := hookEvents(t, f.st); len(evs) != 0 {
		t.Errorf("got %d %s events for an unconfigured hook, want 0", len(evs), RunEvent)
	}
	if _, err := os.Stat(f.path("update")); err == nil {
		t.Error("on_update ran for a publish")
	}
	if f.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", f.stderr.String())
	}
}

func TestRunFailingHookIsReportedAndRecorded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "echo first; echo boom >&2; exit 3"

	res, err := f.runner(cfg, 10*time.Millisecond).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatalf("Run returned %v for a failing hook, want nil", err)
	}
	if res.ExitCode != 3 || res.TimedOut || res.Tail != "first\nboom" {
		t.Errorf("result = %+v", res)
	}
	want := "hook on_publish for g1/a failed (exit 3): first\nboom; re-run it with `mill hook run g1/a`\n"
	if f.stderr.String() != want {
		t.Errorf("stderr = %q\nwant     %q", f.stderr.String(), want)
	}
	_, d := onlyEvent(t, f.st)
	if d.ExitCode != 3 || d.OutputTail != "first\nboom" || d.TimedOut || d.DurationMS != 10 {
		t.Errorf("event data = %+v", d)
	}
}

func TestRunKeepsOutputTail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "i=1; while [ $i -le 30 ]; do echo line$i; i=$((i+1)); done; exit 1"

	res, err := f.runner(cfg, 0).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(res.Tail, "\n")
	if len(lines) != 20 || lines[0] != "line11" || lines[19] != "line30" {
		t.Errorf("tail = %q, want line11..line30", res.Tail)
	}

	cfg.OnPublish = "head -c 10000 /dev/zero | tr '\\0' x; exit 1"
	res, err = f.runner(cfg, 0).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tail) != 4096 {
		t.Errorf("tail is %d bytes, want 4096", len(res.Tail))
	}
}

func TestRunTimeoutKillsHook(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.HookTimeout = config.Duration{Duration: 300 * time.Millisecond}
	cfg.OnPublish = "sleep 5"

	start := time.Now()
	res, err := f.runner(cfg, 5*time.Millisecond).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Run took %s; the hook was not killed at the timeout", elapsed)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("result = %+v, want timed out with exit -1", res)
	}
	if !strings.HasPrefix(f.stderr.String(), "hook on_publish for g1/a failed (timed out after 300ms): ") ||
		!strings.HasSuffix(f.stderr.String(), "; re-run it with `mill hook run g1/a`\n") {
		t.Errorf("stderr = %q", f.stderr.String())
	}
	_, d := onlyEvent(t, f.st)
	if !d.TimedOut || d.ExitCode != -1 {
		t.Errorf("event data = %+v", d)
	}
}

func TestRunTimeoutKillsHookChildren(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.HookTimeout = config.Duration{Duration: 500 * time.Millisecond}
	pidFile := f.path("child.pid")
	cfg.OnPublish = "sleep 30 & echo $! > '" + pidFile + ".tmp'; mv '" + pidFile + ".tmp' '" + pidFile + "'; wait"

	res, err := f.runner(cfg, 0).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("result = %+v, want timed out", res)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(read(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d of the timed-out hook is still running (kill: %v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunPassesOnlyAllowedEnv(t *testing.T) {
	t.Setenv("MILL_TEST_HOOK_ALLOWED", "yes")
	t.Setenv("MILL_TEST_HOOK_SECRET", "leak")
	f := newFixture(t)
	cfg := hookConfig()
	cfg.HookEnv = []string{"MILL_TEST_HOOK_ALLOWED"}
	cfg.OnPublish = "env > '" + f.path("env") + "'"

	if _, err := f.runner(cfg, 0).Run(context.Background(), "publish", "g1", "g1/a", struct{}{}); err != nil {
		t.Fatal(err)
	}
	env := read(t, f.path("env"))
	if !strings.Contains(env, "MILL_TEST_HOOK_ALLOWED=yes\n") {
		t.Errorf("hook_env variable missing from hook env:\n%s", env)
	}
	if strings.Contains(env, "MILL_TEST_HOOK_SECRET") {
		t.Errorf("variable outside the allowlist reached the hook:\n%s", env)
	}
	if !strings.Contains(env, "MILL_HOOK_EVENT=publish\n") {
		t.Errorf("MILL_HOOK_EVENT missing:\n%s", env)
	}
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		k, _, _ := strings.Cut(line, "=")
		switch k {
		case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "LANG", "LC_ALL", "LC_CTYPE",
			"MILL_TEST_HOOK_ALLOWED", "MILL_HOOK_EVENT",
			"PWD", "SHLVL", "_", "OLDPWD": // set by sh itself
		default:
			t.Errorf("unexpected variable %q in hook env", k)
		}
	}
}

func TestCheckSetsCheckEnvWithoutStdin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cmd := "echo \"$MILL_HOOK_CHECK\" > '" + f.path("check") + "'; cat > '" + f.path("stdin") + "'; echo ok"

	tail, err := Check(context.Background(), cfg, f.root, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if tail != "ok" {
		t.Errorf("tail = %q, want ok", tail)
	}
	if got := strings.TrimSpace(read(t, f.path("check"))); got != "1" {
		t.Errorf("MILL_HOOK_CHECK = %q, want 1", got)
	}
	if got := read(t, f.path("stdin")); got != "" {
		t.Errorf("stdin = %q, want empty", got)
	}
}

func TestCheckReportsFailureAndTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()

	tail, err := Check(context.Background(), cfg, f.root, "echo nope; exit 2")
	if err == nil || !strings.Contains(err.Error(), "exit 2") || tail != "nope" {
		t.Errorf("Check = %q, %v; want exit 2 error with tail nope", tail, err)
	}

	cfg.HookTimeout = config.Duration{Duration: 300 * time.Millisecond}
	start := time.Now()
	if _, err := Check(context.Background(), cfg, f.root, "sleep 5"); err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Errorf("Check of a slow hook = %v, want timed out", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Check took %s; the hook was not killed at the timeout", elapsed)
	}
}

func TestRunDeadlinePassedBeforeStartIsTimedOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "touch '" + f.path("ran") + "'"
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	res, err := f.runner(cfg, 0).Run(ctx, "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || strings.Contains(res.Tail, "start sh -c") {
		t.Errorf("result = %+v, want timed out without a start failure", res)
	}
	if msg := f.stderr.String(); !strings.Contains(msg, "timed out after 10s") || strings.Contains(msg, "start sh -c") {
		t.Errorf("stderr = %q", msg)
	}
	_, d := onlyEvent(t, f.st)
	if !d.TimedOut || strings.Contains(d.OutputTail, "start sh -c") {
		t.Errorf("event data = %+v", d)
	}
	if _, err := os.Stat(f.path("ran")); err == nil {
		t.Error("the hook ran after its deadline")
	}

	tail, err := Check(ctx, cfg, f.root, "true")
	if err == nil || !strings.Contains(err.Error(), "timed out after 10s") || strings.Contains(tail, "start sh -c") {
		t.Errorf("Check = %q, %v; want timed out", tail, err)
	}
}

func TestStartFailureWithoutDeadlineIsNotTimedOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.OnPublish = "true"
	f.root = f.path("missing")

	res, err := f.runner(cfg, 0).Run(context.Background(), "publish", "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.ExitCode != -1 || !strings.Contains(res.Tail, "start sh -c") {
		t.Errorf("result = %+v, want a start failure", res)
	}
	_, d := onlyEvent(t, f.st)
	if d.TimedOut || d.ExitCode != -1 {
		t.Errorf("event data = %+v", d)
	}
}
