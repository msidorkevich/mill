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

	"mill/internal/budget"
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

// Request is one unit of agent work.
type Request struct {
	Role   Role
	Prompt string
	Dir    string          // working directory (the task's worktree)
	Schema json.RawMessage // JSON Schema the output must match; nil = free-form JSON
	Tools  []string        // allowed tools; nil = runner/role default
	Model  string          // e.g. "sonnet"; "" = runner default
	Budget budget.Limits
	Count  budget.Count
	// OnUsage, when set, receives each usage increment as the runner observes it.
	OnUsage func(delta budget.Usage)
}

// Status is how a run ended.
type Status string

const (
	StatusOK             Status = "ok"
	StatusError          Status = "error"           // the agent or its process failed
	StatusCancelled      Status = "cancelled"       // the context was cancelled
	StatusBudgetExceeded Status = "budget_exceeded" // stopped because a limit was reached
)

// Result is the outcome of a Request.
type Result struct {
	Status Status
	Output json.RawMessage // JSON matching Request.Schema when Status is ok
	Usage  budget.Usage    // total usage for the run
	Error  string          // human-readable reason when Status is not ok
}

// Runner executes Requests. Implementations must honour ctx cancellation and must report
// usage through Request.OnUsage when it is set.
type Runner interface {
	Name() string
	Run(ctx context.Context, req Request) (Result, error)
}
