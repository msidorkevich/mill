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
      merged)

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
- **Known limitation (resolved by the M2 follow-ups, `cost-estimate`):** a run killed for its
  token limit has no `result` event, so its cost was recorded as $0. It is now estimated from
  tokens and marked `~$`.
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

- [x] Timestamps are stored with RFC3339Nano, which trims trailing zeros, so text ordering by
      `created_at` is wrong for whole-second times (`12:00:00Z` sorts after `12:00:00.1Z`).
      Store a fixed-width format. (Done by mill: task `fixed-timestamps`, 1 attempt, $0.19;
      schema v3 migrates old values.)
- [x] The gate reuses Go's test cache (it ran in 0.8s after the implementer's own run). Fine for
      determinism, but consider `-count=1` in `check_cmd` if a cached pass ever hides a problem.
      (Done: the gate runs `make check GOTESTFLAGS=-count=1`.)

## M2. Planner and graph: done (2026-11-21, branch `m2-graph`)

Requirements: PL1–6, GR1–6, GT2–3, BG2, TS3. Exit criterion: a goal becomes 3–8 tasks, all
verified as local branches; killing and restarting mill finishes with nothing duplicated.

**Dogfooding plan:** mill builds M2 itself. Until the scheduler exists, each piece is a single
task (`mill task run`), reviewed and merged by hand. Once the scheduler is merged, the rest of
M2 runs as a goal through M2's own graph runner. Task files: `~/prj/mill-tasks/`.

### Design decisions

- **Goals** get sequential ids `g1`, `g2`, … and a status: `draft` (plan awaiting approval) →
  `approved` → `running` → `done`, or `blocked` when nothing more can run.
- **Task ids inside a goal** are stored as `<goal>/<task>` (e.g. `g1/config`), so plans from
  different goals never collide. Branch `mill/<goal>/<task>`, worktree
  `<worktrees>/<goal>/<task>`, runs `.mill/runs/<goal>/<task>/`. Single tasks keep
  `mill/t/<id>`.
- **Base rules (GT2):** no deps → `base_branch` tip; one dep → the parent's branch at its
  verified head; several → the deepest parent's branch, with the other parents merged in.
  A merge conflict blocks the task (the keeper handles it in M4).
- **Diff base:** a branch records `base_sha` (parent tip, for restacking later) and `start_sha`
  (after fan-in merges). The verifier reviews `start_sha..HEAD`, so it sees only this task's
  changes.
- **Overlapping `owns`** between tasks with no dependency path are reported as plan warnings;
  the scheduler never runs them at the same time (GR4).
- **Hand-written plans** (`mill plan --file plan.json`) skip the planner. Used for dogfooding
  and tests.
- **Crash recovery:** a run lock (`flock` on `.mill/run.lock`) keeps one mill run per repo. On
  start, tasks left `running` go back to `pending`, attempts left `running` become `abandoned`,
  and uncommitted worktree changes are committed as an interrupted attempt.
- **Project conventions for agents** live in `CLAUDE.md` (workers load it; checked with a probe).

### Subtasks

Built by me (setup):
- [x] Branch `m2-graph`; mill tasks branch from it (`base_branch = "m2-graph"`)
- [x] `CLAUDE.md` with conventions for agents
- [x] Gate runs tests uncached (`make check GOTESTFLAGS=-count=1`)

Built by mill, one task at a time:
- [x] `plan-validate`: `internal/plan`: plan format, validation (PL2–PL3), cycle detection,
      `owns` overlap warnings, topological order, deepest parent (1 attempt, $0.22)
- [x] `goals-store`: goals in the store (create, get, list, status, plan), approving a plan
      creates its tasks and deps in one transaction, `start_sha` on branches (1 attempt, $0.37)
- [x] `plan-cli`: `mill plan --file`, `plan show`, `plan edit`, `plan approve`, `goal list`,
      `goal show` (1 attempt, $0.50)
- [x] `graph-branches`: pipeline for goal tasks: names, base rules with fan-in, `start_sha`
      diffs, goal budget check (GT2–3, BG2) (1 attempt, $0.44)
- [x] `run-safety`: run lock (`flock`), crash recovery in the store, pipeline safe for
      parallel use, interrupted work committed, fake runner per-task scripts and delays
      (1 attempt, $0.55)
- [x] `scheduler`: `mill run <goal>`: ready set, `owns` serialization, `max_parallel`, goal
      status, interruption handling, `--retry-blocked` (GR1–GR5) (1 attempt, Opus, $2.11)

Built by mill as a goal (through the scheduler): goal `g1`, done in one `mill run`, 5/5 tasks
verified on the first attempt, 192.6k tokens, $1.71; two tasks at a time; `workflow-docs`
waited for both of its dependencies and branched from `crash-test` with `planner` merged in.
- [x] `planner`: `mill plan "<goal>"`: repo survey, planner agent, retry once on validation
      errors (PL1, PL4, PL6) (Opus, $1.25)
- [x] `crash-test`: kill -9 a running `mill run` and restart; nothing duplicated (TS3) ($0.18)
- [x] `stream-message-fix`: found in review: `system/permission_denied` stream events carry
      `message` as a string, so the claude runner counted them as unparseable lines ($0.08)
- [x] `scheduler-once`: found in review: a task that returns still `pending` would be restarted
      forever; never start a task twice in one run ($0.11)
- [x] `workflow-docs`: README walkthrough of plan → approve → run (fan-in of `planner` and
      `crash-test`) ($0.09; two inaccurate sentences corrected by hand in review)

Exit criterion (toy Go repo, real agents):
- [x] A real goal through the real planner becomes 3–8 tasks, all verified: `mill plan` made 5
      tasks in one call ($0.13): three independent `strutil` helpers, a three-way fan-in
      `cmd-toy` CLI and a README. All verified; the delivered CLI works.
- [x] Kill and restart finishes with nothing duplicated: `kill -9` while two implementers were
      running; the restart recovered 2 tasks and 2 attempts, committed their finished work as
      interrupted attempts, continued with attempt 2, verified everything, one branch per task,
      nothing verified ran again. Goal total $0.36 (excluding the killed attempts, see below).

### Follow-ups found in M2 reviews

- [ ] A goal task's spec cannot be changed after approval; a blocked goal task can only be
      retried as is (`mill run <goal> --retry-blocked`). Add a way to edit a task's spec.
- [ ] The planner call has no time limit (`runner.attempt_timeout` is only applied in the
      pipeline); a hung planner is stopped only by its budget.
- [ ] Workers run in their own process groups, so `kill -9` of mill does not reach them. In the
      real kill test no `claude` process was left 3s later, but that relies on Claude Code
      exiting by itself. Record each worker's process group and kill any survivors during
      crash recovery.
- [ ] Attempts abandoned by a killed run record no usage (mill never saw their final result),
      so goal and task spend under-count killed runs. Recovery could read the attempt's
      stream file and record the usage it reports.

## M2 follow-ups: done (goal `g2`, on `main`)

`m2-graph` was merged into `main` (fast-forward); mill's tasks now branch from `main`. The
follow-ups run as one goal through mill's scheduler (plan:
`~/prj/mill-tasks/followups.plan.json`).

Correction: the planner follow-up was partly a review mistake. `mill plan` does apply
`runner.attempt_timeout` (I reviewed `planner.go` and missed the wrapper in `cli/plan.go`). The
real gap is smaller: the limit is shared by both planner calls and a timeout reads as a bare
"context deadline exceeded".

Goal `g2`: 5/5 tasks verified in one `mill run`, 321.8k tokens, $1.88. First real retries:
`planner-timeout` and `worker-reaper` each failed the gate once and passed on attempt 2.

- [x] `task-edit`: `mill task edit <id>` changes a pending/blocked/failed task's spec, also after
      approval (goal task spec updated in the task row and the stored plan) ($0.39)
- [x] `planner-timeout`: time limit per planner call, with a clear message ($0.16, 2 attempts)
- [x] `worker-reaper`: record each worker's pid; recovery kills leftover workers of a killed run
      ($0.50, 2 attempts)
- [x] `cost-estimate`: price table (overridable in config); runs that end without a result from
      claude get an estimated cost, marked `~$` (resolves the M1 "killed run costs $0"
      limitation) ($0.52)
- [x] `abandoned-usage`: recovery reads abandoned attempts' streams and records their usage
      ($0.31; recovery line changed by hand to billable tokens and the existing `fmtInt`)

Checked with real agents (toy repo): `kill -9` of mill during a `claude` run left the worker
**alive** (so the M2 observation of no survivors was luck). On retry, recovery printed `killed
leftover worker 7074`, the worker was gone, and the abandoned attempt got its usage from its
stream with an estimated cost (`~$0.01`).

The M2 follow-ups section above is resolved by this goal:
- spec cannot change after approval → `task-edit`
- planner time limit → `planner-timeout`
- workers surviving a killed mill → `worker-reaper`
- abandoned attempts record no usage → `abandoned-usage`

### Follow-ups found in the g2 reviews

- [ ] Both gate failures in g2 came from the crash test written in g1, not from the code under
      change: the test is flaky. `planner-timeout`'s retry fixed nothing (it re-ran until the
      test passed); `worker-reaper` fixed the test. Two races: (1) it killed mill while task a
      could still be running, so a reran (fixed: wait until a is verified); (2) "run lock still
      held" right after the killed process was reaped, seen twice in gates, not reproduced in 21
      local runs under load. The test now polls the lock for up to 10s, which would also hide a
      real "lock outlives its holder" bug. Watch for it.
- [ ] `owns` is advisory: `worker-reaper` changed `internal/cli/crash_test.go`, outside its
      `owns`. Helpful here, but it is the kind of change `owns` exists to catch. Consider
      reporting changes outside `owns` as a finding, or at least showing them to the verifier.
- [ ] The reaper only kills a group whose leader is still alive and matches. If the agent exited
      but a child it started (e.g. a test run from its Bash tool) survives in the group, it is
      left alone.
- [ ] A gate failure caused by a flaky or unrelated test makes the implementer "fix" code that
      was fine, or retry until it passes. The verifier and findings could say when a failing test
      is outside the task's diff.

## g2 review follow-ups: done (goal `g3` + task `reaper-cwd`, on `main`)

Plan: `~/prj/mill-tasks/g3-followups.plan.json`, run through mill's scheduler.

Hypothesis for the "run lock still held" flake: `flock` belongs to the open file, and a child
mill is starting (git, a gate command) shares mill's open files until it execs. If mill is
killed in that window, the half-started child keeps the lock for a moment after mill is gone.
Heavy load (parallel gates, `-race`) widens the window. `fcntl` locks belong to the process and
are not inherited by children, so the lock ends exactly when mill does.

- [x] `runlock-fcntl`: run lock on `fcntl` (process-owned); crash test strict again; stress run
      ($0.19). Review: 24/24 strict crash-test runs passed, four at a time under `-race`. Caveat:
      the old failure never reproduced locally either (0/21), so this shows the strict test is
      reliable, not that the hypothesis was right; it removes the only mechanism found.
- [x] `owns-check`: files changed outside `owns` are listed; `owns_policy` = warn (verifier
      judges) | enforce (finding before the gate) | off ($0.29). Changed in review: the enforce
      finding now says how to revert (`git restore --source <start>`; `git checkout` is denied
      to implementers).
- [x] `reaper-orphans`: recovery also kills members of a worker's process group whose leader
      already exited ($0.16)
- [x] `gate-triage`: a failing gate command is re-run once (flaky → passes, verifier told);
      if it still fails it runs on the task's starting point: fails there too → task blocked as
      not caused by it; passes there → finding says the task caused it (Opus, $1.85)
- [x] `reaper-cwd` (found in review of `reaper-orphans`; the flaw was in my spec): a leaderless
      group can be an unrelated process group if the worker's group died and its pid was
      reused before recovery; only kill members whose working directory is inside the task's
      worktree ($0.24). The first run with owns-check and gate triage active: the agent stayed
      in its owns and the gate passed, so neither path was exercised by a real agent yet.

## Real-agent check of gate triage, and what it found (goal `g4`, on `main`)

On the toy repo, `main` was made red with a failing test, then a small unrelated task ran. Gate
triage worked as designed: one implementer attempt ($0.05), the gate failed, the re-run failed
(not flaky), the same command failed on the starting point, and the task was blocked as not
caused by it, with no wasted attempts. Two gaps showed up:

- [ ] `task-rebase`: the block reason says "fix the base branch, then retry", but a retry keeps
      the task's original starting point (still red), so it blocked again (a second implementer
      run, $0.01). Add `mill task rebase <id>` to move the task's own commits onto the base's
      current tip; the block reason names it.
- [ ] `gitignore-refresh`: repos initialized before `run.lock` existed show it as untracked;
      `mill init` never updates an existing `.mill/.gitignore`. Init adds missing lines, doctor
      warns.
