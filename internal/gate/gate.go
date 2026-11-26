// Package gate runs a task's checks in its worktree (TP3).
//
// A gate passes only when every command ran and exited 0. A command that could not run,
// timed out, or a gate with no commands at all, is a failure (P3).
package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/host"
)

// CmdResult is the outcome of one gate command.
type CmdResult struct {
	Cmd     string `json:"cmd"`
	Exit    int    `json:"exit"`            // -1 when it did not run to completion
	Error   string `json:"error,omitempty"` // why it could not run, or timed out
	LogPath string `json:"log_path"`
	Tail    string `json:"tail"` // last lines of output, for findings and the verifier
	// Note says where the command ran when the gate's host could not start it.
	Note string `json:"note,omitempty"`
	// StartFailed is true when no host could start the command, so it never ran: a re-run
	// or a run on the task's starting point says nothing about it.
	StartFailed bool `json:"start_failed,omitempty"`
}

// Passed reports whether the command ran and exited 0.
func (c CmdResult) Passed() bool { return c.Error == "" && c.Exit == 0 }

// Result is the outcome of a gate run.
type Result struct {
	Passed   bool        `json:"passed"`
	Reason   string      `json:"reason,omitempty"`
	Commands []CmdResult `json:"commands"`
}

// Options configure a gate run.
type Options struct {
	Host host.Host
	// Fallback runs a command Host could not start twice (e.g. the exec host); nil, or the
	// same kind of host as Host, means none.
	Fallback host.Host
	Dir      string        // the worktree
	Cmds     []string      // check_cmd first, then the task's checks; empty entries are skipped
	LogDir   string        // gate-<i>.log files are written here; never overwritten
	Env      []string      // complete environment for the commands
	Timeout  time.Duration // per command
	Nice     int           // > 0 runs each command under `nice -n Nice`; 0 runs it unchanged
}

// TailLines and TailBytes bound how much output is kept for findings.
const (
	TailLines = 40
	TailBytes = 4000
)

// Run executes every command, even after a failure, so findings show the full picture.
func Run(ctx context.Context, o Options) Result {
	var cmds []string
	for _, c := range o.Cmds {
		if strings.TrimSpace(c) != "" {
			cmds = append(cmds, c)
		}
	}
	if len(cmds) == 0 {
		return Result{Passed: false, Reason: "no checks configured: set check_cmd in .mill/config.toml or add checks to the task", Commands: []CmdResult{}}
	}
	res := Result{Passed: true}
	for i, c := range cmds {
		cr := runOne(ctx, o, i+1, c)
		if !cr.Passed() {
			res.Passed = false
		}
		res.Commands = append(res.Commands, cr)
		if ctx.Err() != nil {
			res.Passed = false
			res.Reason = "gate interrupted"
			break
		}
	}
	if !res.Passed && res.Reason == "" {
		var failed []string
		for _, c := range res.Commands {
			if !c.Passed() {
				failed = append(failed, c.Cmd)
			}
		}
		res.Reason = "failed: " + strings.Join(failed, "; ")
	}
	return res
}

func runOne(ctx context.Context, o Options, i int, cmd string) CmdResult {
	cr := CmdResult{Cmd: cmd, Exit: -1, LogPath: uniqueLog(o.LogDir, i)}
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	// Merge stderr into stdout so the log reads in order.
	argv := []string{"/bin/sh", "-c", "exec 2>&1\n" + cmd}
	if o.Nice > 0 {
		argv = append([]string{"nice", "-n", strconv.Itoa(o.Nice)}, argv...)
	}
	h, note, err := start(cctx, o, host.ProcSpec{
		Name:       fmt.Sprintf("gate %d", i),
		Argv:       argv,
		Dir:        o.Dir,
		Env:        o.Env,
		StdoutPath: cr.LogPath,
	})
	cr.Note = note
	if err != nil {
		cr.Error = "could not run: " + err.Error()
		cr.StartFailed = ctx.Err() == nil && !errors.Is(cctx.Err(), context.DeadlineExceeded)
		return cr
	}
	ex, err := h.Wait()
	cr.Tail = tail(cr.LogPath)
	switch {
	case err != nil:
		cr.Error = "could not run: " + err.Error()
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		cr.Error = fmt.Sprintf("timed out after %s", o.Timeout)
	case ctx.Err() != nil:
		cr.Error = "interrupted"
	case ex.Signaled:
		cr.Error = "killed by a signal"
	default:
		cr.Exit = ex.Code
	}
	return cr
}

// start starts spec on o.Host. A start failure is retried once on the same host; when that
// fails too, the command runs on the fallback host and note says so. A failed start never
// ran the command.
func start(ctx context.Context, o Options, spec host.ProcSpec) (h host.Handle, note string, err error) {
	h, err = o.Host.Start(ctx, spec)
	if err == nil || ctx.Err() != nil {
		return h, "", err
	}
	if h, err = o.Host.Start(ctx, spec); err == nil || ctx.Err() != nil {
		return h, "", err
	}
	fb := o.Fallback
	if fb == nil || fb.Name() == o.Host.Name() {
		return nil, "", err
	}
	note = fmt.Sprintf("%s could not start it (twice: %v); ran on %s", o.Host.Name(), err, fb.Name())
	h, ferr := fb.Start(ctx, spec)
	if ferr != nil {
		return nil, note, fmt.Errorf("%s could not start it (twice: %v), nor could %s: %w", o.Host.Name(), err, fb.Name(), ferr)
	}
	return h, note, nil
}

// uniqueLog returns <dir>/gate-<i>.log, or gate-<i>.<k>.log if that exists (TP3: never
// overwrite an earlier run's output).
func uniqueLog(dir string, i int) string {
	p := filepath.Join(dir, fmt.Sprintf("gate-%d.log", i))
	for k := 2; ; k++ {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("gate-%d.%d.log", i, k))
	}
}

func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > TailBytes {
		s = s[len(s)-TailBytes:]
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > TailLines {
		lines = lines[len(lines)-TailLines:]
	}
	return strings.Join(lines, "\n")
}
