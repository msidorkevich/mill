package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"mill/internal/budget"
)

// Step is one scripted response of the fake runner.
type Step struct {
	// Output is returned as Result.Output on success.
	Output json.RawMessage `json:"output"`
	// Usage is reported to OnUsage one increment at a time, checking ctx between increments.
	Usage []budget.Usage `json:"usage,omitempty"`
	// Error, when set, makes the run end with StatusError and this message.
	Error string `json:"error,omitempty"`
	// Files are written into Request.Dir before returning, simulating agent edits.
	Files map[string]string `json:"files,omitempty"`
}

// Script maps each role to the responses it returns, in order.
type Script map[Role][]Step

// Fake is a scripted, zero-token runner for tests (RN2). It is safe for concurrent use.
type Fake struct {
	mu     sync.Mutex
	script Script
	next   map[Role]int
	calls  []Request
}

// NewFake returns a fake runner that plays script.
func NewFake(script Script) *Fake {
	return &Fake{script: script, next: map[Role]int{}}
}

// LoadFakeScript reads a JSON script: {"implementer": [{"output": {...}, ...}], ...}.
func LoadFakeScript(path string) (Script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse fake script %s: %w", path, err)
	}
	return s, nil
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
	steps := f.script[req.Role]
	i := f.next[req.Role]
	if i >= len(steps) {
		f.mu.Unlock()
		return Result{}, fmt.Errorf("%w for role %q (call %d)", ErrScriptExhausted, req.Role, i+1)
	}
	f.next[req.Role] = i + 1
	step := steps[i]
	f.mu.Unlock()

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
	return Result{Status: StatusOK, Output: out, Usage: total}, nil
}
