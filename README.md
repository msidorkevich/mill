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
mill task run --file ../add-reverse.json --retry    # after editing the spec or raising a budget
```

A verified task is a branch `mill/t/<id>` in a worktree outside your checkout; mill prints the
command to review its diff. mill never pushes and never touches `main`.

Exit codes: 0 ok, 1 error, 2 usage error, 3 check failed, 4 task not verified.
