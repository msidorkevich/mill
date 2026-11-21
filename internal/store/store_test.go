package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixed = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, Options{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesSchemaInWALMode(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))

	v, err := s.SchemaVersion(ctx)
	if err != nil || v != LatestVersion {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, LatestVersion)
	}
	mode, err := s.JournalMode(ctx)
	if err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, %v; want wal", mode, err)
	}
	for _, table := range []string{"goals", "tasks", "deps", "attempts", "branches", "events"} {
		var n int
		if err := s.DB().QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	s := open(t, path)
	if _, err := s.AppendEvent(ctx, "init", "", "", nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	evs, err := s2.Events(ctx, 0, 0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("after reopen: %d events, %v", len(evs), err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	s := open(t, path)
	if _, err := s.DB().ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err := Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "newer than this mill") {
		t.Fatalf("err = %v, want refusal of newer schema", err)
	}
}

func TestEventsAppendAndRead(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))

	seq1, err := s.AppendEvent(ctx, "goal-created", "G-1", "", map[string]string{"title": "todos"})
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := s.AppendEvent(ctx, "task-started", "G-1", "T-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if seq2 <= seq1 {
		t.Fatalf("seq not increasing: %d then %d", seq1, seq2)
	}

	evs, err := s.Events(ctx, 0, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("Events = %d, %v", len(evs), err)
	}
	e := evs[0]
	if e.Seq != seq1 || e.Kind != "goal-created" || e.GoalID != "G-1" || e.TaskID != "" || !e.At.Equal(fixed) {
		t.Fatalf("event 1 = %+v", e)
	}
	if string(e.Data) != `{"title":"todos"}` {
		t.Fatalf("data = %s", e.Data)
	}
	if string(evs[1].Data) != "{}" {
		t.Fatalf("nil data stored as %s, want {}", evs[1].Data)
	}

	after, err := s.Events(ctx, seq1, 0)
	if err != nil || len(after) != 1 || after[0].Seq != seq2 {
		t.Fatalf("Events(after %d) = %+v, %v", seq1, after, err)
	}
	limited, err := s.Events(ctx, 0, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit ignored: %d, %v", len(limited), err)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if _, err := s.AppendEvent(ctx, "x", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "UPDATE events SET kind = 'y'"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE err = %v, want append-only refusal", err)
	}
	if _, err := s.DB().ExecContext(ctx, "DELETE FROM events"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE err = %v, want append-only refusal", err)
	}
}

func TestAppendEventRejectsEmptyKind(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if _, err := s.AppendEvent(context.Background(), "", "", "", nil); err == nil {
		t.Fatal("empty kind accepted")
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO deps (task_id, depends_on) VALUES ('nope', 'missing')`)
	if err == nil {
		t.Fatal("dep on missing tasks accepted; foreign keys are off")
	}
}
