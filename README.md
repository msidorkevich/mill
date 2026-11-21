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
```

A verified task is a branch `mill/t/<id>` in a worktree outside your checkout; mill prints the
command to review its diff. mill never pushes and never touches `main`.

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
```

`mill run` starts a task once all its dependencies are verified, up to `max_parallel` at a
time, and never runs two tasks whose `owns` overlap together. Each task gets the branch
`mill/<goal>/<task>`, built on its dependencies' branches. When every task is verified the goal
is `done` (exit 0). When a task blocks or fails, the others continue; the goal then ends
`blocked` with a reason naming the stopped tasks and the ones waiting on them (exit 4). Ctrl-C
lets running tasks stop, returns them to pending and leaves the goal blocked; run `mill run g1`
again to continue. `start_children_on = "published"` is not supported yet; use `"verified"`.

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
   another terminal, with `mill goal show g1`.
6. **Review the branches.** Each verified task is a branch `mill/g1/<task>`. Inspect it with git
   (for example `git log main..mill/g1/<task>` and `git diff main...mill/g1/<task>`), then merge
   it in your forge. mill never pushes and never touches `main`.

**Base branches.** A task with no dependency is based on the base branch. With one dependency it
is based on that dependency's branch. With several, it is based on the deepest dependency's
branch and the others are merged in, so its diff against that base is only its own work.

**Blocked tasks.** When a task blocks or fails, independent tasks still run and the goal ends
`blocked` (exit 4); `mill goal show g1` names the stopped tasks and the ones waiting on them.
Fix the cause (the spec with `mill task show`, a budget or `check_cmd` in `.mill/config.toml`),
then `mill run g1 --retry-blocked` puts blocked and failed tasks back to pending and continues.
Verified tasks are not run again.

**Interrupted or killed runs.** After Ctrl-C, a crash or `kill -9`, nothing is lost. The next
`mill run g1` recovers the half-finished tasks and continues where it stopped. Only one
`mill run` can work on a repository at a time; a second one fails and says so. Wait for the
first to finish or stop it, then run again.

Exit codes: 0 ok, 1 error, 2 usage error, 3 check failed, 4 task or goal not verified.
