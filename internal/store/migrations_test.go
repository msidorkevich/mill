package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openWith opens path with an injected migration list.
func openWith(ctx context.Context, path string, list []migration) (*Store, error) {
	return Open(ctx, path, Options{Now: func() time.Time { return fixed }, migrations: list})
}

// withExtra returns the build's migrations followed by extra, as a newer mill would have.
func withExtra(extra ...migration) []migration {
	list := append([]migration(nil), migrations[:LatestVersion]...)
	return append(list, extra...)
}

func readMinReader(t *testing.T, s *Store) (version, minReader int) {
	t.Helper()
	if err := s.DB().QueryRow(`SELECT version, min_reader FROM schema_min_reader WHERE id = 1`).
		Scan(&version, &minReader); err != nil {
		t.Fatalf("read schema_min_reader: %v", err)
	}
	return version, minReader
}

func TestMigrationsDeclareAdditiveFlag(t *testing.T) {
	t.Parallel()
	// Only migration 3 rewrites existing rows; every other one adds tables or columns.
	want := map[int]bool{3: false}
	if len(migrations) < 14 {
		t.Fatalf("len(migrations) = %d, want >= 14", len(migrations))
	}
	for i, m := range migrations[:14] {
		additive, ok := want[i+1]
		if !ok {
			additive = true
		}
		if m.Additive != additive {
			t.Errorf("migration %d: Additive = %v, want %v", i+1, m.Additive, additive)
		}
	}
}

func TestMigrationRecordsMinimumReaderVersion(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	version, minReader := readMinReader(t, s)
	if version != LatestVersion || minReader != minReaderVersion(migrations[:LatestVersion]) || minReader < 3 {
		t.Fatalf("schema_min_reader = (%d, %d), want (%d, %d)",
			version, minReader, LatestVersion, minReaderVersion(migrations[:LatestVersion]))
	}
}

func TestOlderMillOpensDatabaseWithNewerAdditiveMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	newer := withExtra(migration{Additive: true, SQL: `
CREATE TABLE future_things (id TEXT PRIMARY KEY);
ALTER TABLE goals ADD COLUMN future_note TEXT NOT NULL DEFAULT '';
`})
	s, err := openWith(ctx, path, newer)
	if err != nil {
		t.Fatalf("open with newer list: %v", err)
	}
	before, err := s.CreateGoal(ctx, "written by newer mill", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	old := open(t, path)
	if v, err := old.SchemaVersion(ctx); err != nil || v != LatestVersion+1 {
		t.Fatalf("schema version = %d, %v; want %d (left unmigrated)", v, err, LatestVersion+1)
	}
	if g, err := old.GetGoal(ctx, before.ID); err != nil || g.Title != "written by newer mill" {
		t.Fatalf("GetGoal = %+v, %v", g, err)
	}
	after, err := old.CreateGoal(ctx, "written by older mill", nil)
	if err != nil {
		t.Fatalf("CreateGoal on newer additive schema: %v", err)
	}
	if err := old.SetGoalStatus(ctx, after.ID, "blocked", "test"); err != nil {
		t.Fatalf("SetGoalStatus: %v", err)
	}
	goals, err := old.ListGoals(ctx)
	if err != nil || len(goals) != 2 {
		t.Fatalf("ListGoals = %d goals, %v; want 2", len(goals), err)
	}
	if _, err := old.AppendEvent(ctx, "test", after.ID, "", nil); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	old.Close()

	// The newer mill still opens it, finding nothing to migrate.
	s2, err := openWith(ctx, path, newer)
	if err != nil {
		t.Fatalf("reopen with newer list: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.SchemaVersion(ctx); v != LatestVersion+1 {
		t.Fatalf("schema version = %d, want %d", v, LatestVersion+1)
	}
}

func TestRefusesNewerSchemaWithNonAdditiveMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	newer := withExtra(
		migration{Additive: false, SQL: `UPDATE goals SET title = upper(title);`},
		migration{Additive: true, SQL: `CREATE TABLE future_things (id TEXT PRIMARY KEY);`},
	)
	s, err := openWith(ctx, path, newer)
	if err != nil {
		t.Fatalf("open with newer list: %v", err)
	}
	s.Close()

	_, err = Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "newer than this mill") || !strings.Contains(err.Error(), "upgrade mill") {
		t.Fatalf("err = %v, want refusal asking to upgrade mill", err)
	}
	// A mill that knows the non-additive migration opens it.
	s2, err := openWith(ctx, path, newer[:LatestVersion+1])
	if err != nil {
		t.Fatalf("open with list including the non-additive migration: %v", err)
	}
	s2.Close()
}

func TestRefusesNewerSchemaWithoutMinimumReaderRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	s, err := openWith(ctx, path, withExtra(migration{Additive: true, SQL: `CREATE TABLE future_things (id TEXT PRIMARY KEY);`}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE schema_min_reader`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "upgrade mill") {
		t.Fatalf("err = %v, want refusal asking to upgrade mill", err)
	}
}

func TestMigrationAddsMinimumReaderRecordToExistingDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mill.db")
	s, err := openWith(ctx, path, migrations[:LatestVersion-1])
	if err != nil {
		t.Fatal(err)
	}
	// A database written before the record existed.
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE schema_min_reader`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path)
	version, minReader := readMinReader(t, s2)
	if version != LatestVersion || minReader != minReaderVersion(migrations[:LatestVersion]) {
		t.Fatalf("schema_min_reader = (%d, %d), want (%d, %d)",
			version, minReader, LatestVersion, minReaderVersion(migrations[:LatestVersion]))
	}
}
