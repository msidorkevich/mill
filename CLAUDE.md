# mill: conventions for agents working on this repo

mill is a Go CLI (Go 1.24, module `mill`). Requirements: docs/REQUIREMENTS.md.

## Code
- Match the style of the surrounding code. Standard library first; no new dependencies.
- Package layout: `cmd/mill` (entry), `internal/<pkg>`. CLI commands live in `internal/cli`.
- Errors say what failed and what to do next. Never hide a failure (no ignored errors on
  state-changing calls, no `|| true`).
- A check that cannot decide fails; it never reports success.
- State changes go through `internal/store`; never write SQL outside that package.
- Run `gofmt` on every file you touch.

## Tests
- Name tests by the behaviour they cover (`TestRetryContinuesOnSameBranch`), never after
  milestones or tickets.
- Tests assert outcomes (output, exit codes, DB rows, files), not internals.
- Tests use temporary directories and temporary git repos only. Never touch `~/.mill`, the
  repo's own `.mill/`, `~/.claude` or the network.
- No real agent calls in tests: use the fake runner (`internal/runner/fake.go`).
- Tests are deterministic: inject clocks, no sleeps to wait for state (poll with a deadline).

## Before you finish
- `make check` must pass (gofmt, go vet, go test).
- Document user-visible changes in README.md (CLI usage) when a task adds a command.
