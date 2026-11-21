// Package store is mill's SQLite state: goals, tasks, attempts, branches and an
// append-only event log (ST1–ST3).
//
// Only mill writes this database. Large outputs (logs, diffs, agent results) live as files
// under .mill/runs/ and are referenced by path.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is an open mill database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Options configure Open.
type Options struct {
	// Now returns the current time. Defaults to time.Now. Tests inject a fixed clock.
	Now func() time.Time
}

// Open opens (or creates) the database at path, enables WAL mode and applies pending
// migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer at a time; SQLite serializes writes anyway and this avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: opts.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that own their own queries (and for tests).
func (s *Store) DB() *sql.DB { return s.db }

// JournalMode returns SQLite's journal mode ("wal" when Open succeeded).
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}

// SchemaVersion returns the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// Event is one entry of the append-only log. Seq is the ordering authority, not At.
type Event struct {
	Seq    int64           `json:"seq"`
	At     time.Time       `json:"at"`
	Kind   string          `json:"kind"`
	GoalID string          `json:"goal_id,omitempty"`
	TaskID string          `json:"task_id,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// AppendEvent writes an event and returns its sequence number. Data may be nil.
func (s *Store) AppendEvent(ctx context.Context, kind, goalID, taskID string, data any) (int64, error) {
	if kind == "" {
		return 0, errors.New("event kind must not be empty")
	}
	payload := []byte("{}")
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return 0, fmt.Errorf("marshal event data: %w", err)
		}
		payload = b
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (at, kind, goal_id, task_id, data) VALUES (?, ?, ?, ?, ?)`,
		s.ts(), kind, nullable(goalID), nullable(taskID), string(payload))
	if err != nil {
		return 0, fmt.Errorf("append event %s: %w", kind, err)
	}
	return res.LastInsertId()
}

// Events returns up to limit events with seq > afterSeq, in seq order. limit <= 0 means all.
func (s *Store) Events(ctx context.Context, afterSeq int64, limit int) ([]Event, error) {
	q := `SELECT seq, at, kind, COALESCE(goal_id, ''), COALESCE(task_id, ''), data
	      FROM events WHERE seq > ? ORDER BY seq`
	args := []any{afterSeq}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.queryEvents(ctx, q, args...)
}

func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at, data string
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &e.GoalID, &e.TaskID, &data); err != nil {
			return nil, err
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, fmt.Errorf("event %d: bad timestamp %q: %w", e.Seq, at, err)
		}
		e.At = t
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timeLayout is the one layout the store writes timestamps in: UTC with a 9-digit fraction that
// is never trimmed, so text order equals chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// parseTime reads a stored timestamp in timeLayout or the older RFC3339Nano form.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
