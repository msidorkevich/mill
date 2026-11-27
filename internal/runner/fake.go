package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
)

// Step is one scripted response of the fake runner.
type Step struct {
	// Output is returned as Result.Output on success.
	Output json.RawMessage `json:"output"`
	// Usage is reported to OnUsage one increment at a time, checking ctx between increments.
	Usage []budget.Usage `json:"usage,omitempty"`
	// Error, when set, makes the run end with StatusError and this message.
	Error string `json:"error,omitempty"`
	// Transient, with Error, makes the run end with StatusTransient instead of StatusError.
	Transient bool `json:"transient,omitempty"`
	// Files are written into Request.Dir before returning, simulating agent edits.
	Files map[string]string `json:"files,omitempty"`
	// DelayMS makes the fake wait this long before returning (cancelled early with the context).
	DelayMS int `json:"delay_ms,omitempty"`
	// RateLimit, when set, makes the run end with StatusRateLimited after reporting Usage.
	RateLimit *RateLimit `json:"rate_limit,omitempty"`
	// DurationMS is returned as Result.Duration on success, the agent-reported run time.
	DurationMS int `json:"duration_ms,omitempty"`
}

// RateLimit describes the usage limit a scripted run hits.
type RateLimit struct {
	Type     string    `json:"type,omitempty"`      // e.g. five_hour; "" = unknown
	ResetsAt time.Time `json:"resets_at,omitempty"` // zero = unknown
}

// Script maps each role to the responses it returns, in order.
type Script map[Role][]Step

// FakeScript is a loaded script file: role scripts plus optional per-task scripts. A request
// whose TaskID has a task script plays that script; any other request plays the role script.
type FakeScript struct {
	Roles Script
	Tasks map[string]Script
}

// Fake is a scripted, zero-token runner for tests (RN2). It is safe for concurrent use.
type Fake struct {
	mu    sync.Mutex
	roles Script
	tasks map[string]Script
	next  map[string]int // keyed by task id (or "") and role
	calls []Request
}

// NewFake returns a fake runner that plays a Script (role scripts only) or a FakeScript.
func NewFake[S Script | FakeScript](script S) *Fake {
	f := &Fake{next: map[string]int{}}
	switch v := any(script).(type) {
	case Script:
		f.roles = v
	case FakeScript:
		f.roles, f.tasks = v.Roles, v.Tasks
	}
	return f
}

// LoadFakeScript reads a JSON script: {"implementer": [{"output": {...}, ...}], ...}, with an
// optional "tasks": {"<task id>": {"<role>": [steps]}} for per-task scripts.
func LoadFakeScript(path string) (FakeScript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FakeScript{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return FakeScript{}, fmt.Errorf("parse fake script %s: %w", path, err)
	}
	fs := FakeScript{Roles: Script{}}
	for key, v := range raw {
		if key == "tasks" {
			if err := json.Unmarshal(v, &fs.Tasks); err != nil {
				return FakeScript{}, fmt.Errorf("parse fake script %s: tasks: %w", path, err)
			}
			continue
		}
		var steps []Step
		if err := json.Unmarshal(v, &steps); err != nil {
			return FakeScript{}, fmt.Errorf("parse fake script %s: %s: %w", path, key, err)
		}
		fs.Roles[Role(key)] = steps
	}
	return fs, nil
}

// Name implements Runner.
func (f *Fake) Name() string { return "fake" }

// Calls returns a copy of every request received, in order.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.calls...)
}

// ErrScriptExhausted is returned when a role has no responses left.
var ErrScriptExhausted = errors.New("fake runner: no scripted step left")

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, req Request) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	steps, key := f.roles[req.Role], string(req.Role)
	if ts, ok := f.tasks[req.TaskID]; ok && req.TaskID != "" {
		steps, key = ts[req.Role], req.TaskID+"\x00"+string(req.Role)
	}
	i := f.next[key]
	if i >= len(steps) {
		f.mu.Unlock()
		return Result{}, fmt.Errorf("%w for role %q (call %d)", ErrScriptExhausted, req.Role, i+1)
	}
	f.next[key] = i + 1
	step := steps[i]
	f.mu.Unlock()

	if step.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(step.DelayMS) * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Result{Status: StatusCancelled, Error: ctx.Err().Error()}, nil
		}
	}

	var total budget.Usage
	for _, u := range step.Usage {
		if err := ctx.Err(); err != nil {
			return Result{Status: StatusCancelled, Usage: total, Error: err.Error()}, nil
		}
		total = total.Add(u)
		if req.OnUsage != nil {
			req.OnUsage(u)
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{Status: StatusCancelled, Usage: total, Error: err.Error()}, nil
	}
	if rl := step.RateLimit; rl != nil {
		return Result{Status: StatusRateLimited, Usage: total, Error: "usage limit reached",
			LimitType: rl.Type, ResetsAt: rl.ResetsAt}, nil
	}
	if step.Error != "" && step.Transient {
		return Result{Status: StatusTransient, Usage: total, Error: step.Error}, nil
	}
	if step.Error != "" {
		return Result{Status: StatusError, Usage: total, Error: step.Error}, nil
	}
	if err := writeFiles(req.Dir, step.Files); err != nil {
		return Result{}, err
	}
	out := step.Output
	if out == nil {
		out = json.RawMessage("{}")
	}
	return Result{Status: StatusOK, Output: out, Usage: total,
		Duration: time.Duration(step.DurationMS) * time.Millisecond}, nil
}
