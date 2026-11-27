// Package hook runs the operator's on_publish, on_update and status shell commands.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/store"
)

// RunEvent is the event kind recorded for every hook run.
const RunEvent = "hook-run"

const (
	tailLines = 20
	tailBytes = 4 << 10
	// keepBytes bounds how much output is buffered before the tail is cut.
	keepBytes = 64 << 10
	// waitDelay bounds how long Wait waits for output pipes after the process is gone.
	waitDelay = 2 * time.Second
)

// Result describes one hook run. Skipped is set when no command is configured.
type Result struct {
	Skipped  bool
	ExitCode int
	TimedOut bool
	Tail     string // the tail of stdout and stderr combined
	Duration time.Duration
	// Stdout is what a status hook printed on stdout (up to the last 64 KiB); empty for
	// other events.
	Stdout string
	// Status is the document a status hook printed, set when the run succeeded.
	Status *Status
	// statusErr is why a status hook that exited 0 has no Status.
	statusErr error
}

// Failed reports whether the hook failed: it exited non-zero, timed out, or (for a status
// hook) printed no valid mill/status@1 document.
func (r Result) Failed() bool {
	return r.TimedOut || r.ExitCode != 0 || r.statusErr != nil
}

// Runner runs hooks from the repository root and records each run as a RunEvent.
type Runner struct {
	Store  *store.Store
	Cfg    config.Config
	Root   string
	Stderr io.Writer
	Now    func() time.Time
}

// Run runs the hook configured for event ("publish", "update" or "status") with doc as JSON
// on stdin. A status hook's stdout is parsed as a mill/status@1 document into Result.Status.
// The hook's own failure is reported on Stderr and in the Result; the returned error only
// covers failing to prepare the run or to record the event.
func (r Runner) Run(ctx context.Context, event, goalID, taskID string, doc any) (Result, error) {
	var command string
	switch event {
	case "publish":
		command = r.Cfg.OnPublish
	case "update":
		command = r.Cfg.OnUpdate
	case EventStatus:
		command = r.Cfg.Status
	default:
		return Result{}, fmt.Errorf("unknown hook event %q: use publish, update or status", event)
	}
	if command == "" {
		return Result{Skipped: true}, nil
	}
	name := "hook on_" + event
	if event == EventStatus {
		name = "status hook"
	}
	stdin, err := json.Marshal(doc)
	if err != nil {
		return Result{}, fmt.Errorf("%s for %s: encode stdin document: %w", name, taskID, err)
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}

	env := append(host.WorkerEnv(r.Cfg.HookEnv...), "MILL_HOOK_EVENT="+event)
	start := now()
	out := run(ctx, r.Root, command, env, stdin, r.Cfg.HookTimeout.Duration, event == EventStatus)
	res := Result{
		ExitCode: out.exitCode,
		TimedOut: out.timedOut,
		Tail:     out.tail,
		Duration: now().Sub(start),
		Stdout:   out.stdout,
	}
	if event == EventStatus && !res.TimedOut && res.ExitCode == 0 {
		st, err := ParseStatus([]byte(res.Stdout))
		if err != nil {
			res.statusErr = err
		} else {
			res.Status = &st
		}
	}

	data := map[string]any{
		"event":       event,
		"command":     command,
		"exit_code":   res.ExitCode,
		"duration_ms": res.Duration.Milliseconds(),
		"output_tail": res.Tail,
		"timed_out":   res.TimedOut,
	}
	if res.statusErr != nil {
		data["error"] = res.statusErr.Error()
	}
	if _, err := r.Store.AppendEvent(ctx, RunEvent, goalID, taskID, data); err != nil {
		return res, fmt.Errorf("record %s event for %s of %s: %w", RunEvent, name, taskID, err)
	}

	if r.Stderr == nil {
		return res, nil
	}
	rerun := fmt.Sprintf("re-run it with `mill hook run %s`", taskID)
	if event == EventStatus {
		rerun = "it runs again after status_interval"
	}
	switch {
	case res.TimedOut || res.ExitCode != 0:
		status := fmt.Sprintf("exit %d", res.ExitCode)
		if res.TimedOut {
			status = fmt.Sprintf("timed out after %s", r.Cfg.HookTimeout.Duration)
		}
		fmt.Fprintf(r.Stderr, "%s for %s failed (%s): %s; %s\n", name, taskID, status, res.Tail, rerun)
	case res.statusErr != nil:
		fmt.Fprintf(r.Stderr, "status hook printed no mill/status@1 JSON: %s\n(%s for %s: %v; fix the hook to print a mill/status@1 object on stdout; %s)\n",
			res.Tail, name, taskID, res.statusErr, rerun)
	}
	return res, nil
}

// Check runs cmd the way Run would, with MILL_HOOK_CHECK=1 set and no stdin, for doctor.
// It returns the output tail and an error when the command fails or times out.
func Check(ctx context.Context, cfg config.Config, root, cmd string) (string, error) {
	return check(ctx, cfg, root, cmd, host.WorkerEnv(cfg.HookEnv...))
}

// CheckEvent is Check with MILL_HOOK_EVENT=event also set, for checking a hook that tells
// events apart.
func CheckEvent(ctx context.Context, cfg config.Config, root, cmd, event string) (string, error) {
	return check(ctx, cfg, root, cmd, append(host.WorkerEnv(cfg.HookEnv...), "MILL_HOOK_EVENT="+event))
}

func check(ctx context.Context, cfg config.Config, root, cmd string, env []string) (string, error) {
	env = append(env, "MILL_HOOK_CHECK=1")
	out := run(ctx, root, cmd, env, nil, cfg.HookTimeout.Duration, false)
	switch {
	case out.timedOut:
		return out.tail, fmt.Errorf("hook check %q timed out after %s", cmd, cfg.HookTimeout.Duration)
	case out.exitCode != 0:
		return out.tail, fmt.Errorf("hook check %q failed (exit %d)", cmd, out.exitCode)
	}
	return out.tail, nil
}

type outcome struct {
	exitCode int
	timedOut bool
	tail     string // of stdout and stderr combined
	stdout   string // set only when stdout was captured separately
}

// run executes `sh -c command` in its own process group, killing the group after timeout.
// A nil stdin gives the command no input. With splitStdout, stdout is also kept on its own.
func run(ctx context.Context, dir, command string, env []string, stdin []byte, timeout time.Duration, splitStdout bool) outcome {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	buf := &tailBuffer{}
	var stdout *tailBuffer
	cmd.Stdout = buf
	cmd.Stderr = buf
	if splitStdout {
		stdout = &tailBuffer{}
		// Separate writers make exec copy stdout and stderr concurrently; tailBuffer locks.
		cmd.Stdout = io.MultiWriter(stdout, buf)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return host.KillGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return outcome{exitCode: -1, tail: tail(fmt.Sprintf("start sh -c: %v", err))}
	}
	err := cmd.Wait()
	// Kill anything the hook left running in its group (e.g. backgrounded children).
	killErr := host.KillGroup(cmd.Process.Pid)

	out := outcome{tail: tail(buf.String())}
	if stdout != nil {
		out.stdout = stdout.String()
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		out.exitCode = -1
		out.timedOut = true
		return out
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		out.exitCode = 0
	case errors.As(err, &ee):
		out.exitCode = ee.ExitCode()
	default:
		out.exitCode = -1
		out.tail = appendLine(out.tail, err.Error())
	}
	if killErr != nil {
		if out.exitCode == 0 {
			out.exitCode = -1
		}
		out.tail = appendLine(out.tail, fmt.Sprintf("kill leftover hook processes: %v", killErr))
	}
	return out
}

func appendLine(s, line string) string {
	if s == "" {
		return tail(line)
	}
	return tail(s + "\n" + line)
}

// tail returns the last tailLines lines of s, at most tailBytes bytes, without trailing
// whitespace.
func tail(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	lines := strings.Split(s, "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	s = strings.Join(lines, "\n")
	if len(s) > tailBytes {
		s = s[len(s)-tailBytes:]
	}
	return s
}

// tailBuffer keeps the last keepBytes bytes written to it. It is safe for concurrent writes.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > keepBytes {
		t.b = append([]byte(nil), t.b[len(t.b)-keepBytes:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
