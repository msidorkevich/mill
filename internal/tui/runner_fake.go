package tui

import (
	"context"
	"io"
	"strings"
	"sync"
)

// FakeResult is the scripted outcome of one fake command.
type FakeResult struct {
	Stdout string
	Stderr string
	Code   int
	Err    error // returned by Capture or Wait
	// StartErr makes Start fail.
	StartErr error
	// Running keeps a started command running until it is interrupted or finished.
	Running bool
}

// FakeCall is one recorded call to the FakeRunner.
type FakeCall struct {
	Method string // "Capture" or "Start"
	Args   []string
}

// FakeRunner is a CommandRunner that records every call and returns scripted results. It runs
// no process. Results are keyed by the space-joined args; unscripted commands exit 0 at once.
type FakeRunner struct {
	mu      sync.Mutex
	results map[string]FakeResult
	calls   []FakeCall
	procs   []*FakeProc
}

// NewFakeRunner returns a FakeRunner with no scripted results.
func NewFakeRunner() *FakeRunner { return &FakeRunner{results: map[string]FakeResult{}} }

// Script sets the result of the command with these args.
func (f *FakeRunner) Script(res FakeResult, args ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[strings.Join(args, " ")] = res
}

// Calls returns every call so far, in order.
func (f *FakeRunner) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

// Procs returns every process Start returned, in order.
func (f *FakeRunner) Procs() []*FakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*FakeProc(nil), f.procs...)
}

func (f *FakeRunner) record(method string, args []string) FakeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Method: method, Args: append([]string(nil), args...)})
	return f.results[strings.Join(args, " ")]
}

// Capture implements CommandRunner.
func (f *FakeRunner) Capture(_ context.Context, args []string) (string, string, int, error) {
	r := f.record("Capture", args)
	return r.Stdout, r.Stderr, r.Code, r.Err
}

// Start implements CommandRunner. It writes the scripted stdout and stderr to out.
func (f *FakeRunner) Start(args []string, out io.Writer) (Proc, error) {
	r := f.record("Start", args)
	if r.StartErr != nil {
		return nil, r.StartErr
	}
	if _, err := io.WriteString(out, r.Stdout+r.Stderr); err != nil {
		return nil, err
	}
	p := &FakeProc{args: append([]string(nil), args...), res: r, done: make(chan struct{})}
	if !r.Running {
		p.finish(r.Code)
	}
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	return p, nil
}

// InterruptCode is the exit code of a fake process ended by Interrupt.
const InterruptCode = 130

// FakeProc is a process returned by FakeRunner.Start.
type FakeProc struct {
	args       []string
	res        FakeResult
	mu         sync.Mutex
	interrupts int
	code       int
	finished   bool
	done       chan struct{}
}

// Interrupt records the interrupt and ends a running process with InterruptCode.
func (p *FakeProc) Interrupt() error {
	p.mu.Lock()
	p.interrupts++
	p.mu.Unlock()
	p.finish(InterruptCode)
	return nil
}

// Finish ends a running process with code.
func (p *FakeProc) Finish(code int) { p.finish(code) }

func (p *FakeProc) finish(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	p.code = code
	close(p.done)
}

// Interrupts returns how many times Interrupt was called.
func (p *FakeProc) Interrupts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupts
}

// Wait blocks until the process ends and returns its exit code.
func (p *FakeProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.res.Err
}

// Args implements Proc.
func (p *FakeProc) Args() []string { return p.args }
