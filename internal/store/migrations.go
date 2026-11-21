package store

import (
	"context"
	"fmt"
)

// migrations[i] moves the schema from version i to i+1. Never edit an applied migration;
// append a new one.
var migrations = []string{
	// 1: initial schema.
	`
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
`,
	// 2: task phase and worktree path.
	`
ALTER TABLE tasks ADD COLUMN phase TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN worktree TEXT NOT NULL DEFAULT '';
`,
	// 3: fixed-width timestamps (events are append-only and left alone).
	`
UPDATE goals SET created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN length(created_at) > 20 THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z', updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (created_at IS NULL OR created_at LIKE '____-__-__T__:__:__%Z') AND (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
UPDATE tasks SET created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN length(created_at) > 20 THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z', updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (created_at IS NULL OR created_at LIKE '____-__-__T__:__:__%Z') AND (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
UPDATE attempts SET started_at = substr(started_at,1,19) || '.' || substr(CASE WHEN length(started_at) > 20 THEN substr(started_at,21,length(started_at)-21) ELSE '' END || '000000000',1,9) || 'Z', ended_at = substr(ended_at,1,19) || '.' || substr(CASE WHEN length(ended_at) > 20 THEN substr(ended_at,21,length(ended_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (started_at IS NULL OR started_at LIKE '____-__-__T__:__:__%Z') AND (ended_at IS NULL OR ended_at LIKE '____-__-__T__:__:__%Z');
UPDATE branches SET updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN length(updated_at) > 20 THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z' WHERE (updated_at IS NULL OR updated_at LIKE '____-__-__T__:__:__%Z');
`,
	// 4: why a goal is blocked; the tip a task branch started from.
	`
ALTER TABLE goals ADD COLUMN reason TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN start_sha TEXT NOT NULL DEFAULT '';
`,
}

// LatestVersion is the schema version this build writes.
var LatestVersion = len(migrations)

func (s *Store) migrate(ctx context.Context) error {
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if cur > LatestVersion {
		return fmt.Errorf("database schema version %d is newer than this mill (%d); upgrade mill", cur, LatestVersion)
	}
	for v := cur; v < LatestVersion; v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
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
