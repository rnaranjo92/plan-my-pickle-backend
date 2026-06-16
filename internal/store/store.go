// Package store opens the SQLite database (pure-Go modernc driver) and applies
// the canonical migration. The schema is Postgres-portable for a future swap.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/0001_initial_schema.sql
var schemaSQL string

// DB wraps *sql.DB so the service layer depends on this package, not the driver.
type DB struct {
	*sql.DB
}

// Open opens (and migrates) the database at dsn. Examples:
//
//	store.Open("file:planmypickle.db")          // on-disk
//	store.Open("file:pmp?mode=memory&cache=shared") // in-memory (tests)
func Open(dsn string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serializes writers; one connection keeps PRAGMAs + :memory: stable.
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	if _, err := sqlDB.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		return nil, err
	}
	// Apply the migration only on a fresh database. The migration ends with
	// `PRAGMA user_version = 1`, so a previously-initialized DB reports 1 and we
	// skip it — making server restarts against an existing DB a no-op (instead
	// of failing with "table players already exists").
	var version int
	if err := sqlDB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version == 0 {
		if _, err := sqlDB.Exec(schemaSQL); err != nil {
			return nil, fmt.Errorf("apply migration: %w", err)
		}
	}
	// Forward-fill columns added after a DB was first created. Each ALTER is
	// idempotent (duplicate-column / missing-table errors are ignored), so a
	// fresh DB is a no-op and an older one is upgraded in place — no data loss.
	ensureColumns(sqlDB)
	return &DB{sqlDB}, nil
}

func ensureColumns(db *sql.DB) {
	alters := []string{
		`ALTER TABLE events ADD COLUMN location TEXT`,
		`ALTER TABLE events ADD COLUMN tournament_format TEXT NOT NULL DEFAULT 'round_robin'`,
		`ALTER TABLE events ADD COLUMN dupr_sanctioned INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN dupr_event_id TEXT`,
		`ALTER TABLE events ADD COLUMN admin_passcode TEXT`,
		`ALTER TABLE players ADD COLUMN dupr_id TEXT`,
		`ALTER TABLE players ADD COLUMN dupr_rating REAL`,
		`ALTER TABLE players ADD COLUMN dupr_reliability REAL`,
		`ALTER TABLE brackets ADD COLUMN min_age INTEGER`,
		`ALTER TABLE brackets ADD COLUMN max_age INTEGER`,
		`ALTER TABLE registrations ADD COLUMN bracket_id TEXT`,
		`ALTER TABLE registrations ADD COLUMN check_in_token TEXT`,
		`ALTER TABLE rounds ADD COLUMN bracket_id TEXT`,
		`ALTER TABLE matches ADD COLUMN bracket_id TEXT`,
		`ALTER TABLE matches ADD COLUMN stage TEXT NOT NULL DEFAULT 'pool'`,
		`ALTER TABLE matches ADD COLUMN bracket_round INTEGER`,
		`ALTER TABLE matches ADD COLUMN bracket_slot INTEGER`,
		`ALTER TABLE matches ADD COLUMN feeds_match_id TEXT`,
		`ALTER TABLE matches ADD COLUMN feeds_slot INTEGER`,
		// medal bracket: where a match's LOSER drops to (semifinal -> bronze game)
		`ALTER TABLE matches ADD COLUMN loser_feeds_match_id TEXT`,
		`ALTER TABLE matches ADD COLUMN loser_feeds_slot INTEGER`,
		// new tables added after first release are created idempotently here too
		`CREATE TABLE IF NOT EXISTS shirt_orders (
			id TEXT PRIMARY KEY NOT NULL,
			registration_id TEXT NOT NULL REFERENCES registrations (id) ON DELETE CASCADE,
			size TEXT NOT NULL, name_on_shirt TEXT, number TEXT, color TEXT,
			status TEXT NOT NULL DEFAULT 'requested',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			synced_at TEXT, UNIQUE (registration_id))`,
		`CREATE TABLE IF NOT EXISTS finance_entries (
			id TEXT PRIMARY KEY NOT NULL,
			event_id TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			category TEXT NOT NULL,
			amount_cents INTEGER NOT NULL,
			note TEXT,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`,
		`CREATE TABLE IF NOT EXISTS checklist_items (
			id TEXT PRIMARY KEY NOT NULL,
			event_id TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			checked INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`,
	}
	for _, a := range alters {
		if _, err := db.Exec(a); err != nil {
			msg := err.Error()
			// benign: column already present (fresh DB) or table predates this build
			if !strings.Contains(msg, "duplicate column") &&
				!strings.Contains(msg, "no such table") {
				// other errors are non-fatal for a best-effort upgrade
				continue
			}
		}
	}
}
