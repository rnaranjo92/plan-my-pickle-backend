package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestEnsureColumnsUpgradesOldDb(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "old.db")
	// Simulate a pre-existing DB created by an older schema: a players table
	// without dupr_rating, already stamped version 1 (so 0001 won't re-run).
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE players (id TEXT PRIMARY KEY, full_name TEXT)`); err != nil {
		t.Fatal(err)
	}
	raw.Exec(`PRAGMA user_version = 1`)
	raw.Close()

	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM pragma_table_info('players') WHERE name='dupr_rating'`).Scan(&n)
	if n != 1 {
		t.Fatal("forward-migration did not add dupr_rating to an old players table")
	}
}

func TestReopenExistingDbIsIdempotent(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "pmp.db")
	db1, err := Open(dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.Close()
	// Reopening an already-migrated DB must NOT re-run the migration.
	db2, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen failed (the 'table already exists' bug): %v", err)
	}
	defer db2.Close()
	var tables int
	db2.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables)
	if tables < 12 {
		t.Fatalf("schema not intact after reopen: %d tables", tables)
	}
}

func TestMigrationApplies(t *testing.T) {
	db, err := Open("file:pmp_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var tables int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables < 12 {
		t.Fatalf("want >=12 tables, got %d", tables)
	}

	// foreign keys must be ON for this connection
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys not enabled")
	}

	// the standings view should exist
	var views int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='view' AND name='player_standings'`).Scan(&views); err != nil {
		t.Fatalf("view check: %v", err)
	}
	if views != 1 {
		t.Fatalf("player_standings view missing")
	}
}
