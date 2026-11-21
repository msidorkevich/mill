# mill: requirements

*mill is a working name. Still open: language Go, the first target repo, the final name.*

## 1. Purpose

mill is a deterministic harness that turns a goal into a graph of tasks. It runs a coding agent
for each task in isolation, checks the result mechanically and with an independent reviewer
agent, and delivers each task as its own branch for a human to review and merge in their forge.
**Plain code controls the flow. Models only work inside tasks.**

## 2. Principles

- **P1. Deterministic control.** Scheduling, state changes, gates and git operations are code.
  No LLM decides what runs next.
- **P2. Minimal context.** Each agent gets only the inputs it needs. The verifier never sees the
  implementer's transcript.
- **P3. Evidence over claims.** A task passes only on a real gate run and a verifier verdict.
  A check that couldn't run counts as a failure.
- **P4. A human decides.** Nothing is published without operator approval. mill never writes to
  `main`. Changes reach `main` only when a human merges the PR in GitHub, Bitbucket or another
  forge.
- **P5. No forge dependency.** The core uses only plain `git`. Forges are reached only through
  optional hooks.
- **P6. No herdr dependency.** herdr is an optional way to display workers. Plain subprocesses
  are the default.
- **P7. Your checkout stays yours.** All work happens in mill-owned worktrees.
- **P8. Tests cost nothing.** A fake runner and a local bare remote let the full test suite run
  with zero tokens and no network.

## 3. Functional requirements

### Planning (PL)

- **PL1.** `mill plan "<goal>"` calls a planner agent with a minimal repo survey: file tree,
  README, build files, check command.
- **PL2.** The planner returns JSON matching `plan.schema.json`
  (example: [examples/plan-todos.json](examples/plan-todos.json)).
  - Goal fields: `goal`, `assumptions[]`, `tasks[]`.
  - Task fields: `id`, `title`, `kind` (`implement` / `docs` / `test`), `spec`, `acceptance[]`,
    `checks[]`, `owns[]` (file globs), `depends_on[]`, `size` (`S` / `M` / `L`).
- **PL3.** Validation in code:
  - the output matches the schema, ids are unique slugs, and all dependencies exist;
  - no cycles;
  - every `implement` task has at least one acceptance item and at least one check;
  - acceptance items are checkable (vague ones are rejected);
  - no task is size `L` (it must be split);
  - at most K tasks per goal;
  - overlapping `owns` between tasks with no dependency path between them is reported.
- **PL4.** A failed validation goes back to the planner once, with the errors. A second failure
  goes to the operator.
- **PL5.** The operator reviews, edits and approves the plan before anything runs. `checks`
  proposed by the planner are untrusted until approved.
- **PL6.** `size` decides the model and budget, e.g. S → smaller model with a small budget,
  M → stronger model with a larger budget. This mapping is configurable.

### Graph and scheduling (GR)

- **GR1.** The task dependency graph is stored in SQLite.
- **GR2.** A task is *ready* when all of its dependencies are verified
  (default `start_children_on: verified`, alternative `published`).
- **GR3.** Up to N tasks run in parallel (configurable).
- **GR4.** Two tasks with overlapping `owns` never run at the same time.
- **GR5.** Task statuses:
  `pending → ready → running(implement|gate|verify) → verified → draft → published → landed`,
  with side exits `blocked`, `failed` and `cancelled`. Only the harness changes status.
- **GR6.** Crash recovery: after a restart, interrupted attempts are re-queued, and nothing is
  pushed twice.

### Task pipeline (TP)

- **TP1. Prepare:** create a worktree and branch `mill/<goal>/<task>` from the task's base
  (see GT2).
- **TP2. Implement:** run the implementer agent in the worktree. Its output must match a JSON
  schema: `{summary, files_changed, notes}`.
- **TP3. Gate:** run the project `check_cmd` plus the task's `checks` in the worktree.
  A non-zero exit, a command that couldn't run, or no checks at all for an `implement` task all
  count as FAIL. Output is stored per run and never overwritten.
- **TP4. Verify:** a fresh verifier agent with read-only tools gets the spec, acceptance
  criteria, diff and gate output. Output schema:
  `{verdict: pass|fail, findings: [{file, line, issue}]}`. Findings must point at a file and line.
- **TP5. Retry:** a failed gate or verify starts a new implement attempt with the findings
  attached, up to `max_attempts` (e.g. 3). After that the task is `blocked`.
- **TP6.** Each attempt records its tokens, cost, duration, output and log path.

### Runners (RN)

- **RN1.** Runner interface: `Run(role, prompt, dir, schema, tools, budget) → Result`.
  `budget` carries the USD and token limits (BG1). The runner reports usage as it goes
  (tokens by kind, and cost), and `Result` includes the final usage.
- **RN2.** A `fake` runner (scripted, zero tokens) for tests. It reports scripted usage, so
  budget limits can be tested.
- **RN3.** A `claude` runner:
  `claude -p --output-format stream-json --json-schema … --max-budget-usd … --settings <role settings>`.
  mill reads usage from the stream as it arrives, which enables the token limit. Confirmed in M1:
  each `assistant` event carries `message.usage` (deduplicated per message id), the final
  `result` event carries the authoritative usage, `total_cost_usd` and `structured_output`, and
  `--json-schema` works with stream-json.
- **RN4.** Later: a `codex` runner, e.g. so the verifier uses a different provider than the
  implementer.

### Hosts (HO)

- **HO1.** Host interface: `Start(ProcSpec) → Handle (Wait, Kill)`. Results are always read
  from files, never from the screen.
- **HO2.** An `exec` host (subprocess, the default).
- **HO3.** A `herdr` host: one pane per worker via `herdr pane split` + `herdr pane run`.
  Selected by `--host auto|exec|herdr`; `auto` picks herdr only when it's installed and mill
  runs inside a herdr pane.
- **HO4.** The system is fully functional without herdr.

### Git and branches (GT)

- **GT1.** Only plain git; requires git ≥ 2.38 for `merge-tree --write-tree`.
- **GT2.** Branch base rules:
  - no dependencies → `main`;
  - one dependency → the parent's branch;
  - several (fan-in) → the deepest parent's branch, with the other parents merged in.
- **GT3.** Each task branch records `(base, base_sha)`. All rebases use
  `git rebase --onto <new base> <base_sha> <branch>`.
- **GT4.** A published branch is squashed to **one commit** with trailers `Mill-Task:`,
  `Mill-Goal:`, `Mill-Base:`.
- **GT5.** Pushes always use `--force-with-lease=<ref>:<expected sha>`.
- **GT6.** Merge detection after `git fetch`, in order:
  1. `git merge-base --is-ancestor` (merge commit or fast-forward);
  2. `git cherry` (same patch, so a rebase or squash);
  3. `git log --grep "Mill-Task: <id>"` (trailer survived).
- **GT7.** State can be rebuilt from git alone (branches plus trailers) if the DB is lost.

### Review and publishing (RV)

- **RV1.** A verified task becomes a **draft**: a local, unpushed branch.
- **RV2.** The operator sees the review material: the diff against `base_sha`, the verifier
  report, gate output, spec and acceptance criteria, cost and attempts.
- **RV3.** Operator actions:
  - **approve** → publish;
  - **request changes** → the operator's comments become findings for a new attempt;
  - **reject** → the task is `blocked` and its children are put on hold.
- **RV4.** Publish means squash (GT4), then push to `<remote> review/<task>`.
- **RV5.** Publishing is allowed only if:
  - the gate is green at the current head;
  - the verifier passed;
  - there's no conflict with the base;
  - for a stacked task, the parent is already published;
  - for a fan-in task, all parents other than the base parent have landed.
- **RV6.** A generated description for each task branch: task, goal, position in the stack,
  acceptance checklist, gate result with sha, verifier summary, cost. It's used in the commit
  body and passed to hooks.
- **RV7. Landing is external.** A human merges the PR in the forge. After a fetch, mill detects
  the merge (GT6), marks the task `landed`, and restacks its children (KP2).

### Keeper (KP)

- **KP1.** A reconcile loop over all open task branches. Desired state: the branch contains its
  base's tip, has no conflicts, and its gate is green.
- **KP2.** After a parent lands: rebase the children onto `main` via `base_sha`, update their
  base, re-run the gate, and push if published.
- **KP3.** After a parent gets a new attempt: restack its descendants top-down and re-run their
  gates. The verifier is not re-run after a clean restack.
- **KP4.** Lazy rebasing: rebase only on a real conflict or before publishing.
- **KP5.** Conflicts are detected with `git merge-tree --write-tree <base> <branch>`, with no
  checkout.
- **KP6.** Conflict handling:
  1. retry with `git rerere`;
  2. if that fails, a **resolver agent** gets only the conflicting hunks, both diffs and both
     task specs, followed by the gate and the verifier;
  3. if that fails, the operator.
- **KP7.** A gate that turns red after a restack starts a **fixer agent** with the failure log.
  This counts toward the task's attempt cap.
- **KP8. Combined check:** merge `main` plus all ready branches onto a throwaway worktree and
  run the gate. If it's red, bisect to find the conflicting pair and flag both tasks.
- **KP9.** An optional external CI signal comes only through the `status` hook.

### Hooks (HK)

- **HK1.** Optional scripts configured in `.mill/config.toml`: `on_publish`, `on_update`,
  `status`. `on_publish` is the natural place to open a draft PR in the forge.
- **HK2.** Hooks receive JSON on stdin: task, branch, base, head sha, title, description,
  verifier report.
- **HK3.** A missing hook is skipped. A failing hook is reported but never changes task state.
- **HK4.** Example GitHub and GitLab hooks ship *outside* the core.

### Budgets (BG)

- **BG1. Per-attempt limit, in USD, tokens, or both.** When both are set, whichever is reached
  first stops the attempt.
  - USD: passed to the runner (`--max-budget-usd` for claude) and also checked by mill.
  - Tokens: enforced by mill from the runner's live usage reports (RN3). When the limit is
    crossed, mill kills the worker process.
  - Which tokens count is configurable: `billable` (input + output + cache writes, the default)
    or `all` (also cache reads, which grow quickly in long agent sessions).
- **BG2. Per-task and per-goal limits, in USD, tokens, or both.** Enforced by the scheduler before
  dispatch: an attempt is started only if the remaining task and goal budget covers that
  attempt's limit.
- **BG3.** A hard cap on attempts per task, including resolver and fixer runs.
- **BG4.** Spend is shown per attempt, task and goal, in both USD and tokens (broken down into
  input, output, cache write, cache read).
- **BG5. Reaching a limit.** The attempt ends as `budget_exceeded` and the task becomes `blocked`
  with the reason. There is no automatic retry, since the next attempt would most likely hit the
  same limit. The operator can raise the limit and retry.
- **BG6.** Limits are set in `.mill/config.toml` as defaults, per `size`, and can be overridden
  per goal or per task:

  ```toml
  [budget]
  count = "billable"            # or "all"

  [budget.attempt]              # defaults; per-size values below override them
  usd = 2.0
  tokens = 400_000

  [budget.size.S]
  attempt = { usd = 1.0, tokens = 200_000 }
  [budget.size.M]
  attempt = { usd = 3.0, tokens = 600_000 }

  [budget.task]
  usd = 8.0
  [budget.goal]
  tokens = 6_000_000
  ```

### State and storage (ST)

- **ST1.** SQLite in WAL mode. Tables: `goals`, `tasks`, `deps`, `attempts`, `branches`,
  `events`.
- **ST2.** `events` is append-only, with an ordering sequence number for each event.
- **ST3.** Large outputs (logs, diffs, agent results) are files under `.mill/runs/`, referenced
  from the DB.

### Security and permissions (SE)

- **SE1.** A settings file per role:
  - the verifier gets no Edit or Write;
  - no role gets `git push` (only mill pushes);
  - no role gets access to `.mill/`.
- **SE2. Branch-push allowlist:** mill pushes only `review/*` branches. Any push to another ref,
  `main` included, is refused in code, not just by convention.
- **SE3.** Workers start with an allowlisted environment and run inside their worktree.
- **SE4.** This is documented as a guardrail against mistakes, not a security sandbox.
  Containers are a later option.

### CLI (CL)

- **CL1.** Commands: `init`, `doctor`, `plan`, `plan show|edit|approve`, `run`, `status`,
  `task show|retry|skip|cancel`, `review approve|changes|reject`, `keeper`.
- **CL2.** A `--json` flag on read commands, and stable exit codes.

### TUI (UI)

- **UI1.** A TUI (Bubble Tea or Ratatui) that runs in any terminal.
- **UI2.** Views:
  - plan/graph with statuses;
  - review queue (bottom-up);
  - task detail (diff, verifier, gate, spec, attempts, cost);
  - live log tail.
- **UI3.** Keys: `a` approve, `c` request changes (`$EDITOR`), `x` reject, `r` retry, `s` skip,
  `p` pause, `o` open worktree (a new herdr pane if available, otherwise a shell), `l` log.

### Testing (TS)

- **TS1.** Unit tests use the fake runner, so they cost no tokens.
- **TS2.** Git scenarios run against a temporary repo plus a `git init --bare` remote, with a
  test script playing the forge:
  - merge commit, rebase and squash merges;
  - conflicts;
  - a semantic conflict that only the combined check catches.
- **TS3.** A crash/restart test that kills mill mid-run.

## 4. Non-goals for now

- Forge APIs in the core.
- Any push to or merge into `main` by mill.
- Clusters or multiple machines.
- A web UI.
- Signed releases and version pinning.
- Containers.

## 5. Open decisions

1. **Language:** Go + Bubble Tea
2. **First test repo.**
3. **Name:** "hivemill" is already taken by another project.
