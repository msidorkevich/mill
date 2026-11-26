package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migration is one schema step. Additive marks a migration that only adds tables, columns
// or indexes, so a mill that predates it can still read and write the database.
type migration struct {
	Additive bool
	SQL      string
}

// migrations[i] moves the schema from version i to i+1. Never edit an applied migration;
// append a new one, and mark it Additive only if an older mill can ignore it safely.
var migrations = []migration{
	// 1: initial schema.
	{Additive: true, SQL: `
CREATE TABLE goals (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	status     TEXT NOT NULL,
	plan       TEXT NOT NULL DEFAULT '{}',   -- approved plan JSON
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE tasks (
	id         TEXT PRIMARY KEY,
	goal_id    TEXT REFERENCES goals(id),
	title      TEXT NOT NULL,
	kind       TEXT NOT NULL,
	spec       TEXT NOT NULL,
	acceptance TEXT NOT NULL DEFAULT '[]',   -- JSON array of strings
	checks     TEXT NOT NULL DEFAULT '[]',   -- JSON array of commands
	owns       TEXT NOT NULL DEFAULT '[]',   -- JSON array of globs
	size       TEXT NOT NULL DEFAULT 'S',
	status     TEXT NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',     -- why blocked/failed
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE deps (
	task_id    TEXT NOT NULL REFERENCES tasks(id),
	depends_on TEXT NOT NULL REFERENCES tasks(id),
	PRIMARY KEY (task_id, depends_on),
	CHECK (task_id <> depends_on)
);

CREATE TABLE attempts (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id            TEXT NOT NULL REFERENCES tasks(id),
	n                  INTEGER NOT NULL,            -- attempt number within the task
	role               TEXT NOT NULL,               -- implementer, verifier, resolver, fixer
	status             TEXT NOT NULL,               -- running, ok, failed, budget_exceeded, ...
	started_at         TEXT NOT NULL,
	ended_at           TEXT,
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cost_usd           REAL    NOT NULL DEFAULT 0,
	output_path        TEXT NOT NULL DEFAULT '',    -- file under .mill/runs/
	log_path           TEXT NOT NULL DEFAULT '',
	UNIQUE (task_id, n, role)
);

CREATE TABLE branches (
	task_id       TEXT PRIMARY KEY REFERENCES tasks(id),
	name          TEXT NOT NULL UNIQUE,   -- mill/<goal>/<task>
	base          TEXT NOT NULL,          -- base branch name
	base_sha      TEXT NOT NULL,          -- base tip the branch was built on (GT3)
	head_sha      TEXT NOT NULL DEFAULT '',
	published_ref TEXT NOT NULL DEFAULT '',
	updated_at    TEXT NOT NULL
);

CREATE TABLE events (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,  -- ordering authority (ST2)
	at      TEXT NOT NULL,
	kind    TEXT NOT NULL,
	goal_id TEXT,
	task_id TEXT,
	data    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX events_task ON events(task_id, seq);

-- The event log is append-only.
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
`},
	// 2: task phase and worktree path.
	{Additive: true, SQL: `
ALTER TABLE tasks ADD COLUMN phase TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN worktree TEXT NOT NULL DEFAULT '';
`},
	// 3: fixed-width timestamps (events are append-only and left alone).
	{Additive: false, SQL: `
UPDATE goals SET created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN length(created_at) > 20 THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z', updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (created_at IS NULL OR created_at LIKE '____-__-__T__:__:__%Z') AND (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
UPDATE tasks SET created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN length(created_at) > 20 THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z', updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (created_at IS NULL OR created_at LIKE '____-__-__T__:__:__%Z') AND (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
UPDATE attempts SET started_at = substr(started_at,1,19) || '.' || substr(CASE WHEN length(started_at) > 20 THEN substr(started_at,21,length(started_at)-21) ELSE '' END || '000000000',1,9) || 'Z', ended_at = substr(ended_at,1,19) || '.' || substr(CASE WHEN length(ended_at) > 20 THEN substr(ended_at,21,length(ended_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (started_at IS NULL OR started_at LIKE '____-__-__T__:__:__%Z') AND (ended_at IS NULL OR ended_at LIKE '____-__-__T__:__:__%Z');
UPDATE branches SET updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
`},
	// 4: why a goal is blocked; the tip a task branch started from.
	{Additive: true, SQL: `
ALTER TABLE goals ADD COLUMN reason TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN start_sha TEXT NOT NULL DEFAULT '';
`},
	// 5: the worker process behind a running attempt.
	{Additive: true, SQL: `
ALTER TABLE attempts ADD COLUMN pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN command TEXT NOT NULL DEFAULT '';
`},
	// 6: attempt cost estimated from tokens because claude reported none.
	{Additive: true, SQL: `
ALTER TABLE attempts ADD COLUMN cost_estimated INTEGER NOT NULL DEFAULT 0;
`},
	// 7: the sha published for review and the head the gate passed at.
	{Additive: true, SQL: `
ALTER TABLE branches ADD COLUMN published_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN gate_sha TEXT NOT NULL DEFAULT '';
`},
	// 8: operator approval of a verified task and the attempt it approved.
	{Additive: true, SQL: `
ALTER TABLE tasks ADD COLUMN approved_at TEXT NULL;
ALTER TABLE tasks ADD COLUMN approved_attempt INTEGER NULL;
`},
	// 9: short-lived per-task claims held by a mill command while it works on the tasks.
	{Additive: true, SQL: `
CREATE TABLE task_claims (
	task_id    TEXT PRIMARY KEY REFERENCES tasks(id),
	owner      TEXT NOT NULL,                -- Claimer.Owner: pid and a random nonce
	pid        INTEGER NOT NULL,
	command    TEXT NOT NULL DEFAULT '',     -- mill subcommand, e.g. "run"
	executable TEXT NOT NULL DEFAULT '',     -- argv[0] of the claiming process
	claimed_at TEXT NOT NULL
);
`},
	// 10: task complexity and model override; the model each attempt ran with and why.
	{Additive: true, SQL: `
ALTER TABLE tasks ADD COLUMN complexity TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN complexity_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN model_reason TEXT NOT NULL DEFAULT '';
`},
	// 11: gate slots, bounding the gates that run at once across mill processes.
	{Additive: true, SQL: `
CREATE TABLE gate_slots (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id     TEXT NOT NULL,
	attempt     INTEGER NOT NULL,
	pid         INTEGER NOT NULL,
	acquired_at TEXT NOT NULL
);
`},
	// 12: passed gate results, reused when the same tree is gated with the same checks.
	{Additive: true, SQL: `
CREATE TABLE gate_results (
	tree_sha    TEXT NOT NULL,               -- git tree the gate passed on
	fingerprint TEXT NOT NULL,               -- GateFingerprint of check_cmd and the task's checks
	task_id     TEXT NOT NULL,               -- the task whose gate passed
	result      TEXT NOT NULL DEFAULT '{}',  -- the passing gate result JSON
	passed_at   TEXT NOT NULL,
	PRIMARY KEY (tree_sha, fingerprint)
);
`},
	// 13: what a claim's holder is doing with the task, and since when.
	{Additive: true, SQL: `
ALTER TABLE task_claims ADD COLUMN activity TEXT NOT NULL DEFAULT '';
ALTER TABLE task_claims ADD COLUMN activity_since TEXT NULL;
`},
	// 14: how long the agent reports it worked on an attempt; NULL when unknown.
	{Additive: true, SQL: `
ALTER TABLE attempts ADD COLUMN agent_ms INTEGER NULL;
`},
}

// LatestVersion is the schema version this build writes.
var LatestVersion = len(migrations)

// minReaderTable holds one row: the lowest schema version that can still read and write the
// database (min_reader), and the schema version that wrote it. It lives outside the
// migration list so every build can create and read it.
const minReaderTable = `
CREATE TABLE IF NOT EXISTS schema_min_reader (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	version    INTEGER NOT NULL,
	min_reader INTEGER NOT NULL
);
`

// minReaderVersion is the highest version among the non-additive migrations in list: an
// older mill at or above it understands every change the database has seen.
func minReaderVersion(list []migration) int {
	min := 0
	for i, m := range list {
		if !m.Additive {
			min = i + 1
		}
	}
	return min
}

// migrate brings the database up to len(list). A database newer than list opens unmigrated
// when its recorded minimum reader version is at most len(list); otherwise it is refused.
func (s *Store) migrate(ctx context.Context, list []migration) error {
	latest := len(list)
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if cur > latest {
		min, ok, err := s.recordedMinReader(ctx, cur)
		if err != nil {
			return fmt.Errorf("read minimum reader schema version: %w", err)
		}
		if ok && latest >= min {
			return nil
		}
		return fmt.Errorf("database schema version %d is newer than this mill (%d); upgrade mill", cur, latest)
	}
	for v := cur; v < latest; v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, list[v].SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, minReaderTable); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: create schema_min_reader: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_min_reader (id, version, min_reader) VALUES (1, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET version = excluded.version, min_reader = excluded.min_reader`,
			v+1, minReaderVersion(list[:v+1])); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: record minimum reader version: %w", v+1, err)
		}
		// PRAGMA does not accept bound parameters; v is an int we control.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("set schema version %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", v+1, err)
		}
	}
	return nil
}

// recordedMinReader returns the minimum reader version the database records; ok is false
// when the database predates the record or the record was not written at schema version
// cur, so it cannot vouch for the current schema.
func (s *Store) recordedMinReader(ctx context.Context, cur int) (min int, ok bool, err error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_min_reader'`).Scan(&n); err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	var version int
	err = s.db.QueryRowContext(ctx,
		`SELECT version, min_reader FROM schema_min_reader WHERE id = 1`).Scan(&version, &min)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if version != cur {
		return 0, false, nil
	}
	return min, true, nil
}
