# mill

A deterministic harness that turns a goal into a graph of tasks, runs a coding agent for each
task in isolation, checks the result with your project's gate and an independent verifier, and
delivers each task as its own branch for you to review and merge in your forge.

Status: **M1 (single-task pipeline)**. See [docs/PROGRESS.md](docs/PROGRESS.md).

- Requirements: [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md)
- Milestones: [docs/MILESTONES.md](docs/MILESTONES.md)

## Build and test

Needs Go 1.24+ and git 2.38+.

```sh
make check     # gofmt, go vet, go test (zero tokens, no network)
make build     # bin/mill
make install   # go install (only from a merged main)
```

## Use

```sh
cd your-repo
mill init      # creates .mill/ (config.toml, .gitignore, mill.db, runs/)
mill doctor    # checks git, repo, config, database, runner, host; --json for scripts
```

Set `check_cmd` in `.mill/config.toml` (your project's gate, e.g. `make check`), then write a
task file outside the checkout:

```json
{
  "id": "add-reverse",
  "title": "Add Reverse to strutil",
  "kind": "implement",
  "spec": "Add func Reverse(s string) string to package strutil. Reverse by runes, not bytes.",
  "acceptance": ["Reverse(\"abc\") returns \"cba\"", "A table-driven test covers it"],
  "checks": ["go test ./strutil/"],
  "owns": ["strutil/reverse.go", "strutil/reverse_test.go"],
  "size": "S"
}
```

```sh
mill task run --file ../add-reverse.json            # implement → gate → verify → retry
mill task show add-reverse                          # status, attempts, spend, findings (--json)
mill task list                                      # every task, oldest first (--status S, --json)
mill task run --file ../add-reverse.json --retry    # after editing the spec or raising a budget
mill task edit <id> [--file spec.json]              # change a pending, blocked or failed task's spec
mill task rebase <id>                                # move a pending, blocked or failed task's own commits onto its base's current tip
```

`mill task edit` opens the task's current spec (id, title, kind, spec, acceptance, checks, owns,
depends_on, size) in `$EDITOR`, or reads `--file`. Running and verified tasks are refused. The id
and `depends_on` cannot change. An invalid edit prints the problems and keeps the file.

`mill task rebase <id>` fixes a task that gate triage blocked because a check also fails on its
starting point: after you fix the base branch, it replays only the task's own commits (from its
recorded start) onto the base branch's current tip, or, for a goal task with one dependency, onto
that verified dependency's branch head. A task with several dependencies is restacked: its branch
is reset to its deepest dependency's branch head (or to the remote base branch tip when that
dependency landed), the other dependencies that have not landed are merged again, and its own
commits (the non-merge commits after its recorded start) are replayed on top; the commit after
the merges becomes its new starting point. It prints the old and new starting points and the next
step (`mill task run --file <file> --retry`, or `mill run <goal> --retry-blocked`). It refuses
running and verified tasks, tasks without a branch, a worktree with uncommitted changes, and
dependencies that are not verified, published or landed. On a conflict it aborts, leaves the
branch unchanged, lists the conflicting files and exits 1. A branch that already contains the new
tip (or every dependency) is reported as up to date and left alone.

A verified task is a branch `mill/t/<id>` in a worktree outside your checkout; mill prints the
command to review its diff. mill pushes only `review/<task>` branches, on `mill review approve`, and never touches `main`.

### Plans and goals

A goal is a request broken into tasks with dependencies. Let the planner agent write the plan
from a goal text, or write it by hand (see `docs/examples/plan-todos.json`: a `goal`, optional
`assumptions`, and `tasks` that each have `id`, `title`, `kind`, `spec`, `acceptance`,
`checks`, `owns`, `depends_on` and `size`):

```sh
mill plan "add a --json flag to list"   # the planner writes a plan; creates draft goal g1, prints the planning cost
mill plan --file plan.json      # validate, create draft goal g1; prints every problem and creates nothing if invalid
mill plan show g1               # tasks in dependency order; checks are untrusted until approved (--json)
mill plan edit g1               # edit in $EDITOR (default vi); or: mill plan edit g1 --file plan.json
mill plan approve g1            # create the goal's tasks; the plan can no longer be edited
mill goal list                  # ID, STATUS, TASKS (verified/total), TOKENS, COST, TITLE (--json)
mill goal show g1               # tasks in plan order with status, dependencies, attempts, spend (--json)
mill run g1                     # run the approved goal's tasks in dependency order
mill run g1 --retry-blocked     # after fixing the cause: blocked and failed tasks go back to pending
mill run g1 --wait-on-limit     # on a usage limit, wait for the reset and continue instead of stopping
mill watch [g1|g1/a]            # running attempts: task, attempt, role, elapsed, tokens, cost, latest action; redraws every 2s (--once, --interval 5s)
mill review                     # verified and published tasks, oldest first: ID, STATUS, ATTEMPTS, COST, TITLE, BRANCH (--json)
mill review show g1/a           # spec, acceptance, last verdict and gate, attempts, cost, branch, diff stat (--diff, --json)
mill review changes g1/a -m "text"   # operator comments (blank line = one finding) for the next attempt; task goes pending (or --file)
mill review reject g1/a -m "why"     # block the task; its non-landed descendants go on hold
mill review approve g1/a        # publish: one squashed commit force-pushed (with lease) to review/g1/a on the remote
mill review approve --goal g1   # approve every verified task, parents first; children of unpublished parents are queued
mill sync                       # after merges on the forge: mark landed, restack, re-gate and re-publish children (--json)
mill clean                      # remove worktrees and local branches of landed tasks (--remote, --dry-run)
```

`mill sync` fetches the remote and marks each published task landed when its published commit
is in `<remote>/<base_branch>` (method `ancestor`), a commit there has the same patch (`patch`,
after a squash or rebase), or a commit there carries its `Mill-Task: <id>` trailer (`trailer`).
A verified or published single-dependency task whose parent landed is rebased onto
`<remote>/<base_branch>`; one whose parent got new commits is rebased onto the parent's new
head. The gate is re-run (not the verifier) and published tasks are re-published with the lease.
A conflict or a red gate blocks the task with the files or commands, leaving its branch as it
was. A fan-in task whose dependencies moved or landed is restacked like `mill task rebase` does
(onto its deepest dependency, or the remote base branch once that landed, with the other
dependencies that have not landed merged again); a conflict blocks it and leaves its branch
unchanged. A goal whose tasks have all landed becomes `landed`.

`mill run` starts a task once all its dependencies are verified, up to `max_parallel` at a
time, and never runs two tasks whose `owns` overlap together. Each task gets the branch
`mill/<goal>/<task>`, built on its dependencies' branches. When every task is verified the goal
is `done` (exit 0). When a task blocks or fails, the others continue; the goal then ends
`blocked` with a reason naming the stopped tasks and the ones waiting on them (exit 4). Ctrl-C
lets running tasks stop, returns them to pending and leaves the goal blocked; run `mill run g1`
again to continue. `start_children_on = "published"` is not supported yet; use `"verified"`.

`owns_policy` in `.mill/config.toml` sets what happens when an attempt changes files outside the
task's `owns` (a task with empty `owns` is never restricted):

- `"warn"` (default): mill records an `owns-outside` event and the verifier is given the list; it
  passes the task only if the spec or acceptance criteria require each file.
- `"enforce"`: each such file becomes a mill finding for the next attempt and the gate does not run
  for that attempt.
- `"off"`: no check.

When gate commands fail, mill triages them before handing them to the implementer (not when no
checks are configured, and not when the run was interrupted):

1. Re-run (`gate_rerun = true`, the default): only the failed commands run once more (logs under
   `.mill/runs/<task>/a<n>/rerun/`). If they all pass, the gate passes, mill records a
   `gate-flaky` event and the verifier is told which commands were flaky, so it fails the task if
   the change itself could make them nondeterministic.
2. Base check (`gate_check_base = true`, the default): the commands still failing run in a
   temporary detached worktree at the task's starting point, with the same environment and
   timeout (logs under `a<n>/base/`); the worktree is removed afterwards.
   - If every one of them fails there too, the task is blocked in the gate phase with a reason
     naming them: the task did not cause them. Mill records `gate-base-failing` and makes no
     further attempt; fix the commands on the base branch, then `--retry`.
   - Otherwise each command that passes on the starting point becomes a finding saying the task's
     changes cause it, and each that also fails there becomes a finding saying it is not caused by
     the task and should not be fixed.
   - If the base worktree cannot be created or the commands cannot run there, mill logs a warning
     and uses the plain gate findings.

Set either setting to `false` to skip that step.

If an `$EDITOR` result is invalid, mill prints the problems and keeps the edited file; fix it and
run `mill plan edit g1 --file <path>`.

`mill plan "<goal>"` gives the planner agent a short survey of the repository (tracked files,
the head of README.md, build files, `check_cmd`) and read-only tools (Read, Glob, Grep). It uses
the model and per-attempt budget of size M from `.mill/config.toml`, the same agent
environment as `mill run`, cannot touch `.mill/`, and logs under `.mill/runs/plans/<timestamp>/`.
An invalid plan goes back to the planner once with its problems. If the second plan is invalid
too, mill prints the problems, saves the plan to `.mill/runs/plans/<timestamp>/plan.json`,
creates no goal and exits 1: fix the file and run `mill plan --file <that path>`.

### Goal workflow: from plan to verified branches

The commands are listed above; this is the order to use them in.

1. **Plan.** `mill plan "<goal>"` (or `mill plan --file plan.json` for a hand-written plan)
   creates a draft goal, say `g1`.
2. **Review.** `mill plan show g1` lists the tasks in dependency order. Read the `checks`: they
   are commands mill will run, and are untrusted until you approve.
3. **Edit.** `mill plan edit g1` changes the draft. Repeat show and edit until the plan is right.
4. **Approve.** `mill plan approve g1` creates the tasks and freezes the plan.
5. **Run.** `mill run g1` runs the tasks in dependency order. Check progress any time, from
   another terminal, with `mill goal show g1`, or watch the running agents live with
   `mill watch g1`: one line per running attempt with elapsed time, billable tokens, estimated
   cost and the agent's latest action. It redraws every `--interval` (default 2s) until Ctrl-C;
   with `--once`, or when stdout is not a terminal, it prints once and exits.
6. **Review the branches.** Each verified task is a branch `mill/g1/<task>`. Inspect it with git
   (for example `git log main..mill/g1/<task>` and `git diff main...mill/g1/<task>`), then merge
   it in your forge. mill never pushes and never touches `main`.

**Base branches.** A task with no dependency is based on the base branch. With one dependency it
is based on that dependency's branch. With several, it is based on the deepest dependency's
branch and the others are merged in. Either way the verifier reviews only the task's own work:
the diff from where the task started, after those merges.

**Blocked tasks.** When a task blocks or fails, independent tasks still run and the goal ends
`blocked` (exit 4); `mill goal show g1` names the stopped tasks and the ones waiting on them.
Find the cause with `mill task show g1/<task>` (reason, attempts, last findings), fix it (for
example a budget or `check_cmd` in `.mill/config.toml`), then `mill run g1 --retry-blocked` puts
blocked and failed tasks back to pending and continues. To change a stopped goal task's spec, run
`mill task edit g1/<task>` (it updates the task and the stored plan; the dependencies stay fixed
once the plan is approved), then `mill run g1 --retry-blocked`.
Verified tasks are not run again.

**Usage limits.** When an agent run hits the account's usage limit (for example Claude's
`five_hour` or `seven_day` limit), mill does not burn attempts on it. The attempt is recorded as
`rate_limited` with the usage it reported, no findings are recorded and no further attempt is
made; the task is blocked with a reason such as `rate limited: the five_hour usage limit was
reached; resets at 12:50; run again after that` (local time; "the reset time is unknown" when the
agent did not say). Because the limit is account-wide, `mill run` then starts no new task, lets
the running ones finish, and ends the goal blocked (exit 4) with the reset time and the hint
`mill run g1 --retry-blocked`, which puts the rate-limited tasks back to pending. With
`mill run g1 --wait-on-limit` mill instead prints one line with the time it will resume, waits
until the reset time plus one minute (15 minutes when the reset time is unknown), puts the
rate-limited tasks back to pending and continues; Ctrl-C during the wait stops like any other
interruption. `mill task run` ends a rate-limited task blocked (exit 4) with the same reason, and
a rate-limited `mill plan "<goal>"` prints the limit and its reset time and creates no goal.

**Interrupted or killed runs.** After Ctrl-C, a crash or `kill -9`, nothing is lost. The next
`mill run g1` recovers the half-finished tasks and continues where it stopped. Only one
`mill run` can work on a repository at a time; a second one fails and says so. Wait for the
first to finish or stop it, then run again.

### Reviewing, publishing and syncing

Once a goal's tasks are verified, you review them in mill and publish them for your forge.

- `mill review` lists verified and published tasks. `mill review show g1/a` prints the spec,
  acceptance, last verdict and gate, attempts, cost, branch and diff stat (`--diff` for the full
  diff).
- `mill review approve g1/a` publishes the task. `mill review changes g1/a -m "text"` records
  your comments (a blank line separates findings) and sends the task back for another attempt.
  `mill review reject g1/a -m "why"` blocks the task and puts its non-landed descendants on hold.

**The approval queue.** Approving a verified task whose parent is not published or landed yet
does not fail: mill records the approval and prints
`approved g1/b: queued, will publish when parent g1/a is published or landed` (exit 0). Other
refusals (red gate, conflict, wrong status) still fail and record nothing. `mill review` shows
a queued task with status `approved` and what it waits for (WAITING FOR, `waiting` in JSON);
`mill review show` prints when and at which attempt it was approved. Each `mill keep` pass
publishes queued tasks whose dependencies now allow it. An approval covers the code it was given
for, so it is cleared when the task changes: `mill review changes`, `mill review reject`, a task
put back to pending (`--retry-blocked`, crash recovery) or a new implementer or fixer attempt
(including the fixer `mill keep` runs after a red gate). `mill keep` also clears an approval
whose attempt is older than the task's latest one (for example after the resolver agent changed
the code). Restacks keep the approval: a rebase that replays the same commits onto a moved base
is still the approved change.

The recommended loop for a stack: `mill review approve --goal g1` publishes what it can and
queues the rest, then `mill keep --watch` publishes each queued child as soon as you merge its
parent on the forge.

**What a published branch contains.** `review/g1/a` on the remote is one squashed commit with
the task's whole change, not its attempt history. The message holds a description (title,
spec, acceptance) and trailers: `Mill-Task: g1/a`, `Mill-Goal`, and for a stacked task
`Mill-Base` (the parent's published commit). The commit is stacked on its parent: the first
parent of `review/g1/b` is `review/g1/a`'s commit, so a forge shows only b's own change.
A task whose dependencies have not been published is not pushed (it is queued, see above), and
a fan-in task waits for its other dependencies to land. Approving again force-pushes with a lease.

**Syncing.** After you merge on the forge, `mill sync` fetches and detects what landed, by
`ancestor` (merge commit), `patch` (squash or rebase merge with the same patch) or `trailer`
(the `Mill-Task` trailer survived an edited merge). Children of a landed task are restacked
onto `<remote>/<base_branch>`, re-gated and re-published, so their review branches track main.
When every task of a goal has landed, the goal becomes `landed`.

**Cleaning.** `mill clean` removes the worktree and local `mill/*` branch of every landed task,
clears its recorded worktree path and records a `task-cleaned` event; the branch row and history
stay. Tasks that are not landed are never touched. `--remote` also deletes the task's
`review/*` branch on the remote, but only when it still points at the published commit: a branch
the forge already deleted is skipped, and one that moved is left alone with a message.
`--dry-run` prints what would be removed and changes nothing. It ends with a summary (worktrees,
branches, remote branches, skipped with reasons). Running it again is harmless.

### Keeping branches mergeable: `mill keep`

```
mill keep [<goal>] [--watch] [--interval 5m] [--combined] [--json]
```

`mill keep` keeps verified and published branches mergeable while main moves. It takes the run
lock (recovering after a crash, like `mill sync`), so it refuses to start while another `mill`
command holds it. One pass does the following:

- **Fetches and syncs** like `mill sync`: detects merged reviews and marks them landed.
- **Rebases lazily.** Only branches that would conflict with their base are rebased; the others
  are left alone. Fan-in tasks are restacked onto their moved dependencies.
- **Reuses recorded resolutions (rerere).** Conflicts that were resolved before are replayed.
- **Asks the resolver agent** for the conflicts rerere does not know.
- **Fixes red gates.** When a restacked branch's gate turns red, the fixer agent runs.
- **Re-publishes** published tasks whose branch changed, so `review/*` follows.
- **Publishes the approval queue** last, dependencies first: each approved task is published as
  `mill review approve` would. One still refused only because of a dependency stays queued and
  is listed under Waiting with the reason; any other refusal or error blocks the task with a
  reason naming the publish failure.
- **Runs the combined check** when at least two branches are ready (or with `--combined`): all
  ready branches are merged onto `<remote>/<base_branch>` in a throwaway worktree and the gate
  runs there. When it fails, mill reports the culprit and its partner with the failed commands.
  Neither task is blocked.

The summary lists Landed, Rebased, Resolved by rerere, Resolved by agent, Fixed, Republished,
Published from queue, Waiting (with the reason), Blocked (with the reason; inspect with `mill task show <id>`) and Combined check, or prints
`Nothing to do.`. A pass that completes exits 0, even with blocked tasks.

Flags:

- `<goal>`: only keep this goal's tasks. An unknown goal fails and lists the existing goals.
- `--watch`: repeat the pass every `--interval` until interrupted (Ctrl-C), then exit 0. The
  lock is released between passes.
- `--interval <duration>`: time between passes with `--watch` (default `5m`, at least `10s`).
- `--combined`: only fetch and run the combined check; nothing is rebased.
- `--json`: print one document per pass with `"schema": "mill/keep@1"`, holding the keeper
  result (`keeper`, null with `--combined`) and the `combined` result. Progress goes to stderr.
  `keeper.queue` holds `published` (`task_id`, `ref`, `sha`) and `waiting` (`task_id`,
  `reason`); both are empty arrays when nothing is queued.

**mill only ever pushes `review/*` branches.** It never pushes to `main` or any other branch;
merging stays with you and your forge.

Exit codes: 0 ok, 1 error, 2 usage error, 3 check failed, 4 task or goal not verified.
