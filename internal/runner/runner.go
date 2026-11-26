// Package runner defines how mill asks an agent to do one piece of work (RN1).
//
// A Runner turns a Request (role, prompt, working directory, output schema, allowed tools,
// budget) into a Result whose Output is JSON matching the schema. Runners report usage as it
// arrives through Request.OnUsage, so mill can enforce budgets while the agent works and stop
// it by cancelling the context.
package runner

import (
	"context"
	"encoding/json"
	"time"

	"github.com/msidorkevich/mill/internal/budget"
)

// Role names the job an agent does.
type Role string

// Roles used by the pipeline and the keeper.
const (
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleVerifier    Role = "verifier"
	RoleResolver    Role = "resolver"
	RoleFixer       Role = "fixer"
)

// Started describes the process a runner started for a Request.
type Started struct {
	PID     int
	Command string // base name of the executable, e.g. "claude"
	RawPath string // file the agent's raw output is written to
}

// Request is one unit of agent work.
type Request struct {
	Role Role
	// TaskID is the task this run works for ("" outside a task, e.g. planning).
	TaskID string
	Prompt string
	Dir    string          // working directory (the task's worktree)
	Schema json.RawMessage // JSON Schema the output must match; nil = free-form JSON
	Tools  []string        // allowed tools; nil = runner/role default
	Model  string          // e.g. "sonnet"; "" = runner default
	Budget budget.Limits
	Count  budget.Count
	// OnUsage, when set, receives each usage increment as the runner observes it.
	OnUsage func(delta budget.Usage)
	// OnStart, when set, is called right after a runner starts an agent process.
	OnStart func(Started)
	// Name labels the run (log lines, pane titles), e.g. "add-reverse a1 implementer".
	Name string
	// LogDir receives the run's files: prompt, raw agent output, stderr, settings.
	LogDir string
	// ProtectedPaths are absolute directories the agent must not read or write, such as the
	// repository's .mill/ (SE1).
	ProtectedPaths []string
	// Env is the agent process's complete environment (SE3).
	Env []string
	// TmpDir is the agent's TMPDIR. It must lie outside the repository and outside every
	// ProtectedPaths entry, so the role settings never deny the agent its own scratch space.
	TmpDir string
}

// Status is how a run ended.
type Status string

const (
	StatusOK             Status = "ok"
	StatusError          Status = "error"           // the agent or its process failed
	StatusCancelled      Status = "cancelled"       // the context was cancelled
	StatusBudgetExceeded Status = "budget_exceeded" // stopped because a limit was reached
	StatusRateLimited    Status = "rate_limited"    // the account hit a usage limit
)

// Result is the outcome of a Request.
type Result struct {
	Status Status
	Output json.RawMessage // JSON matching Request.Schema when Status is ok
	Usage  budget.Usage    // total usage for the run
	// CostEstimated is true when Usage.CostUSD was estimated from tokens because the agent
	// reported no cost.
	CostEstimated bool
	Error         string // human-readable reason when Status is not ok
	// Warnings are non-fatal observations, e.g. tool calls the permission rules denied.
	Warnings []string
	// LimitType names the usage limit (five_hour, seven_day, ...) when Status is rate_limited;
	// empty when unknown.
	LimitType string
	// ResetsAt is when the usage limit resets when Status is rate_limited; zero when unknown.
	ResetsAt time.Time
	// RawPath is the file holding the agent's raw output, when there is one.
	RawPath string
	// Duration is how long the agent reports it worked; 0 when unknown.
	Duration time.Duration
}

// Runner executes Requests. Implementations must honour ctx cancellation and must report
// usage through Request.OnUsage when it is set.
type Runner interface {
	Name() string
	Run(ctx context.Context, req Request) (Result, error)
}
