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

## Dogfood cut (next)

- [ ] Worktree and branch per task
- [ ] implement → gate → verify → retry with findings, capped at `max_attempts`
- [ ] `claude` runner with `--json-schema` and `--max-budget-usd`
- [ ] Settings file per role (verifier read-only, no `git push`, no `.mill/` access)
- [ ] `mill task run --file task.json`, `mill task show <id>`
- [ ] Done when: a hand-written task on mill's own repo produces a verified branch worth merging
