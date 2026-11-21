# mill: progress

Tracks finished subtasks per milestone. Milestones and requirement IDs:
[MILESTONES.md](MILESTONES.md), [REQUIREMENTS.md](REQUIREMENTS.md).

## M0. Foundations: done (2026-11-21)

- [x] Repo skeleton: Go module, layout, Makefile (`make check`), `.gitignore`
- [x] `internal/budget`: limits in USD and/or tokens, usage, `billable`/`all` counting (BG1 groundwork)
- [x] `internal/config`: load and validate `.mill/config.toml`, defaults, per-size budgets (BG6)
- [x] `internal/project`: find the repo root and `.mill/` paths
- [x] `internal/store`: SQLite in WAL mode, versioned migrations, schema, append-only event log (ST1–3)
- [x] `internal/runner`: runner interface and scripted `fake` runner (RN1–2)
- [x] `internal/host`: host interface, `exec` host, environment allowlist (HO1–2, SE3 groundwork)
- [x] `internal/doctor`: environment checks
- [x] CLI: `mill init`, `mill doctor [--json]`, `mill version`, stable exit codes (CL1 partial, CL2)
- [x] Exit criterion: `make check` green with zero tokens spent (TS1); also green under `go test -race`

### Notes and decisions

- **Toolchain:** Go 1.24. Dependencies are pinned to versions that build with it
  (`modernc.org/sqlite` v1.40.1, `github.com/BurntSushi/toml` v1.5.0); newer sqlite releases
  require Go 1.26.
- **Module path** is `mill` for now; rename when the repo gets a remote.
- **SQLite driver** is `modernc.org/sqlite` (pure Go, no CGO).
- **Config is strict:** unknown keys are errors, so a typo never silently falls back to a default.
  A file that sets `[sizes]` or `[budget.size]` replaces that whole map.
- **Event log** is append-only, enforced by SQLite triggers; `seq` is the ordering authority.
- **Exec host:** each worker runs in its own process group; Kill and context cancellation kill the
  whole group. A nil environment means an empty one, never mill's own (SE3).
- **Fake runner** reports usage in increments and stops when the context is cancelled, so budget
  enforcement (M1) can be tested without tokens. It can also write files to simulate agent edits.
- **Doctor** never spends tokens: it runs `claude --version` but does not check the login. It never
  creates the database. `host.kind = herdr` (or `auto` inside herdr) is reported as a warning
  until the herdr host exists (M5); exec is used meanwhile.
- **No forge CI:** `make check` is the gate (fmt, vet, test).

### Not in M0 (on purpose)

- No queries for tasks, attempts or branches yet: the tables exist, their APIs come with M1/M2.
- No `claude` runner, worktrees or pipeline: dogfood cut / M1.

## M1. Single-task pipeline: done (2026-11-21, branch `m1-pipeline`)

Includes the dogfood cut. Requirements: TP1–6, RN3, SE1, SE3, BG1, BG3, BG5–6.

- [x] Probe `claude -p` stream-json: usage per `assistant` event, final `result` with
      `structured_output`, `total_cost_usd`; `--json-schema` works with stream-json
- [x] Config: `worktree_root`, `gate_timeout`, `env_allow`, `runner.claude_bin`,
      `runner.attempt_timeout`
- [x] `internal/taskspec`: task file format (one task of a plan), strict validation
- [x] Store: migration 2 (task phase, worktree path); tasks, attempts, branches, per-task usage
- [x] `internal/gitx`: worktrees, commit (mill identity fallback, no hooks), diff; ignores
      inherited `GIT_*` env
- [x] `internal/gate`: run `check_cmd` + task checks in the worktree, per-command logs (TP3)
- [x] Prompts and output schemas for implementer and verifier (TP2, TP4)
- [x] `claude` runner: stream-json parsing, live usage, structured output (RN3)
- [x] Role settings: implementer and verifier permissions, no `git push`, no `.mill/` (SE1, SE3)
- [x] `internal/pipeline`: prepare → implement → commit → gate → verify → retry (TP1–6)
- [x] Budgets: per-attempt USD/token limits with kill, task budget check, `budget_exceeded`
      → blocked (BG1, BG3, BG5)
- [x] CLI: `mill task run --file F [--retry]`, `mill task show ID [--json]`
- [x] Exit criterion: real task on a toy repo reaches verified; low token limit blocks the task
      (2026-11-21, toy Go repo, Sonnet: `add-reverse` verified in 1 attempt, 18.2k tokens,
      $0.08; `budget-probe` with a 1k-token limit killed after 2.3s and blocked)
- [x] Fix found by the real run: a finished run whose final usage correction crossed the limit
      was reported as killed; a `result` event now wins over a late cancel
- [x] Dogfood: a hand-written task on mill's own repo produces a verified branch worth merging
      (`task-list`: adds `mill task list`; verified in 1 attempt, 44.8k tokens, $0.24; reviewed:
      one aggregate SQL query, tests for ordering, aggregation, filters, empty and invalid status;
      branch `mill/t/task-list`, waiting to be merged)

### Notes and decisions

- **Workers run with** `--setting-sources project --strict-mcp-config --no-session-persistence
  --permission-mode dontAsk --permission-prompts none`: the operator's user plugins, hooks and
  MCP servers stay out, and anything the role settings don't allow is denied, never prompted.
- **Tools:** implementer `Bash, Read, Edit, Write, Glob, Grep`; verifier `Read, Glob, Grep`
  (the diff and gate results are in its prompt). No subagents, no web.
- **Deny rules** (a tripwire, SE4): git commit/push/reset/checkout/switch/rebase/merge/branch/
  worktree/tag/remote/config, sudo, mill, and any access to the repo's `.mill/`.
- **Worktrees** live outside the repo: `~/.cache/mill/worktrees/<repo>-<hash>/<task>` by default
  (`worktree_root`). Branch `mill/t/<task>` until goals exist.
- **mill commits** after every implement attempt (trailers `Mill-Task`, `Mill-Attempt`), even
  after a failed run, so the next attempt builds on it. Squashing comes with publishing (M3).
- **Usage:** live token counts come from `assistant` events (deduplicated per message id); the
  final `result` event is authoritative and corrects them. Cost only arrives in the `result`
  event, so live USD enforcement is claude's own `--max-budget-usd`.
- **Known limitation:** a run killed for its token limit has no `result` event, so its cost is
  recorded as $0 (tokens are exact). USD totals under-count killed runs. A price table could
  estimate them later.
- **Budget stop rules:** limit crossed while running → killed, `budget_exceeded`, task blocked,
  no automatic retry (BG5). Limit crossed only by the final correction of a finished run → the
  work is kept and a warning is logged.
- **Retry:** `mill task run --file F --retry` reruns a blocked/failed task with the (possibly
  edited) spec, on the same branch and base, continuing attempt numbers, starting from the last
  findings.
- **Exit codes:** 4 = task ran but ended blocked or failed.
- **mill on mill:** `.mill/config.toml` is committed with `check_cmd = "make check"` and
  `base_branch = "m1-pipeline"` (switch to `main` once M1 is merged). Task files live outside the
  repo, in `~/prj/mill-tasks/`.

### Follow-ups found while dogfooding

- [ ] Timestamps are stored with RFC3339Nano, which trims trailing zeros, so text ordering by
      `created_at` is wrong for whole-second times (`12:00:00Z` sorts after `12:00:00.1Z`).
      Store a fixed-width format.
- [ ] The gate reuses Go's test cache (it ran in 0.8s after the implementer's own run). Fine for
      determinism, but consider `-count=1` in `check_cmd` if a cached pass ever hides a problem.
