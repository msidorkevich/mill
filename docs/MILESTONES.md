# mill: milestones

Requirement IDs refer to [REQUIREMENTS.md](REQUIREMENTS.md).

**The MVP is M0–M3**: planning, execution, review, publishing and merge detection, all from the
CLI. M4 makes parallel work safe at scale, M5 makes it pleasant to use, M6 connects forges, more
agent runners and Java + Gradle projects.

## Dogfood gate

The goal is to have mill build mill as early as possible. Only the **dogfood cut** (all of M0
plus a slice of M1) is written by hand. Every remaining requirement becomes a task file that
mill runs on its own repo.

### Dogfood cut (built by hand)

From M0:

- Go module, config loading, SQLite with a minimal schema (`tasks`, `attempts`, `events`).
- Runner interface, `fake` runner, `exec` host.

From M1:

- Worktree and branch per task.
- implement → gate → verify → retry with findings, capped at `max_attempts`.
- `claude` runner with `--json-schema` and `--max-budget-usd` only (the token limit comes later).
- Settings file per role: the verifier gets no Edit or Write, no role gets `git push`, no role
  can access `.mill/`.
- `mill task run --file task.json` and `mill task show <id>`.

**Done when:** a hand-written task on mill's own repo produces a verified branch you'd actually
merge.

### What's done by hand until mill can do it

| Normally provided by | During early dogfooding |
|---|---|
| Planner (M2) | write the task file yourself, using the same schema as one task in a plan |
| Graph and parallel tasks (M2) | run one task at a time |
| Review and publishing (M3) | `git diff main...mill/t/<task>`, then `git push` and open the PR yourself |
| Merge detection, restacking, keeper (M3–M4) | not needed with one PR at a time |
| TUI (M5) | `mill task show` plus the log files under `.mill/runs/` |

### Safety rules for working on mill with mill

1. **Run the installed binary, never the code under change.** `go install` to `~/go/bin/mill`
   (or similar), and update it only after merging. A broken branch can never break the running
   mill.
2. **Tests never touch the live `.mill/`.** Every test uses a temp dir and a temp repo. This
   rule applies from the first commit, because agents run mill's tests inside their worktrees.
3. **The gate is `go vet ./... && go test ./...`** in the task's worktree (plus `golangci-lint`
   if used).
4. **`.mill/runs/` is in `.gitignore`.**

### First tasks for mill to build itself

Pure code with clear acceptance criteria comes first:

1. **BG1 token limit:** read usage from stream-json and kill the worker on the limit. Testable
   with the fake runner.
2. **PL2–PL3:** plan schema and validator (cycles, overlapping `owns`, rule checks).
3. **`mill doctor`:** check the git version, `claude` login, config.
4. **GR1–GR5:** graph and scheduler. From here mill takes whole goals instead of single tasks.
5. Then M3, M4 and M5 in order. After M2, hand mill full goals such as "implement M3".

## M0. Foundations

- Repo skeleton, config loading (`.mill/config.toml`).
- SQLite schema and append-only event log.
- Runner interface and `fake` runner.
- Host interface and `exec` host.
- `mill init`, `mill doctor`.

**Requirements:** ST1–3, RN1–2, HO1–2, CL1 (partial), TS1
**Exit criterion:** CI green with zero tokens spent.

## M1. Single-task pipeline

- Worktree and branch preparation.
- Implement → gate → verify → retry with findings.
- `claude` runner with JSON schemas.
- Settings file per role, allowlisted environment.
- Per-attempt budget in USD and/or tokens (live usage from the stream, kill on limit), attempt
  cap, `budget_exceeded` handling.

**Requirements:** TP1–6, RN3, SE1, SE3, BG1, BG3, BG5–6
**Exit criterion:** a real task on a toy repo goes implement → gate fail → retry → verify pass;
an attempt with a deliberately low token limit is stopped and the task is blocked with the
reason.

## M2. Planner and graph

- Plan schema, planner call, validator, approval from the CLI.
- Scheduler with N parallel tasks and `owns` serialization.
- Branch base rules, including fan-in.
- Per-task and per-goal budgets.
- Crash recovery.

**Requirements:** PL1–6, GR1–6, GT2–3, BG2, TS3
**Exit criterion:** a goal becomes 3–8 tasks, all verified as local branches; killing and
restarting mill finishes with nothing duplicated.

## M3. Review, publish and merge detection

- Draft review from the CLI: approve / request changes / reject.
- Squash to one commit with trailers, push `review/<task>` with lease.
- Publish rules (gate, verifier, conflicts, stack and fan-in order).
- Branch-push allowlist (`review/*` only, never `main`).
- Merge detection after fetch; restack children after a merge.

**Requirements:** GT1, GT4–7, RV1–7, KP2–3, SE2, TS2 (merge cases)
**Exit criterion:** the full flow works against a local bare remote, with merges done by a test
script that plays the forge (merge commit, rebase, squash), and mill detects each one.

## M4. Keeper

- Reconcile loop with lazy rebasing.
- Conflict detection via `git merge-tree`, `git rerere`.
- Resolver agent and fixer agent.
- Combined check with bisect.

**Requirements:** KP1, KP4–8, TS2 (conflict cases)
**Exit criterion:** scenario tests with injected textual and semantic conflicts are resolved or
flagged correctly.

## M5. TUI and herdr

- Graph view, review queue, task detail, live log tail, key bindings.
- Optional herdr host.

**Requirements:** UI1–3, HO3–4
**Exit criterion:** the whole workflow works from the TUI, with and without herdr.

## M6. Extensions

- Hooks (`on_publish`, `on_update`, `status`) plus example GitHub and Bitbucket scripts outside
  the core: POSIX `sh` with `git`, `curl` and `jq` over the forges' REST APIs, no forge CLI. The scripts open one draft PR per published task (or update the open one), targeting
  the parent task's review branch so a stack reviews bottom-up, retarget it when the parent
  lands, and name the parent PR in the body. This is the first goal of M6: everything after it is
  reviewed through PRs the hook opens.
- External CI signal via the `status` hook (GitHub checks, Bitbucket Pipelines build statuses).
- `opencode` runner (any provider opencode supports) and `codex` runner. The runner is chosen per
  role, so the verifier can use a different provider than the implementer.
- Java + Gradle projects: a toolchain adapter for the check command, narrow test checks,
  failing-test triage from JUnit XML reports, added skipped tests, and Gradle caches and daemons
  across worktrees.
- Spend reports.

**Requirements:** HK1–4, KP9, RN4–5, TC1–4, BG4
**Exit criteria:**

- a stack of three tasks gets three PRs from the GitHub hook, each showing only its own commits,
  and merging them bottom-up lands all three with no compare links and zero forge code in the
  core; the same works on Bitbucket;
- a goal on a sample Java + Gradle repo runs from plan to published branches, with an `opencode`
  implementer and a `claude` verifier.
