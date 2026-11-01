package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationsTable is golang-migrate's bookkeeping table (C-SCHEMA-3: the
// vendored runner and its table are the only migration mechanism; there is no
// PRAGMA user_version logic anywhere in this repo).
const migrationsTable = "schema_migrations"

// Open opens the SQLite database at path, fails closed on a dirty migration,
// applies any pending migrations, and returns the single writer pool.
//
// The returned *sql.DB is capped at one connection (C-CONV-9, C-CONV-3): every
// mutation goes through it, so `BEGIN IMMEDIATE` serialises all writers in this
// process. Callers that need concurrent reads use OpenReadPool.
func Open(path string) (*sql.DB, error) {
	// The migration runner gets its own connection, closed before the writer
	// pool exists, so the writer pool never inherits a half-finished
	// transaction or the runner's temporary state.
	if err := runMigrations(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("failed to open database for app: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open database for app: %w", err)
	}
	return db, nil
}

// runMigrations applies pending migrations, refusing to proceed when the
// database is flagged dirty.
func runMigrations(path string) error {
	migrateDB, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer migrateDB.Close()

	// C-SCHEMA-19: a dirty database is never served from, never re-migrated
	// automatically, never ignored and never cleared by us. The check runs
	// before the runner is constructed so the failure is our named error and
	// not the vendored driver's opaque one.
	if err := checkNotDirty(migrateDB, path); err != nil {
		return err
	}

	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to create migration source: %w", err)
	}
	defer source.Close()

	driver, err := sqlite.WithInstance(migrateDB, &sqlite.Config{MigrationsTable: migrationsTable})
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		m.Close()
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return dirtyError(int64(dirty.Version), path)
		}
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}

// checkNotDirty reads the migration bookkeeping row and fails closed when it
// carries dirty = 1 (C-SCHEMA-19). A database with no bookkeeping table has
// never been migrated and is therefore not dirty.
func checkNotDirty(db *sql.DB, path string) error {
	var version int64
	var dirty bool
	err := db.QueryRow(`SELECT version, dirty FROM `+migrationsTable+` LIMIT 1`).Scan(&version, &dirty)
	switch {
	case err == nil:
		if dirty {
			return dirtyError(version, path)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		// The table exists but is empty: no version has been recorded yet.
		return nil
	case isMissingTable(err):
		return nil
	default:
		return fmt.Errorf("failed to read migration state: %w", err)
	}
}

// isMissingTable reports whether err is SQLite's "no such table" for the
// migration bookkeeping table. The driver exposes no typed error for this, and
// the message text is stable for a missing table on every SQLite build.
func isMissingTable(err error) bool {
	return strings.Contains(err.Error(), "no such table: "+migrationsTable)
}

// dirtyError is the single line C-SCHEMA-19 requires: it names the version and
// the only action that clears the flag.
func dirtyError(version int64, path string) error {
	return fmt.Errorf(
		"database %s is at migration version %d with dirty=1: a previous migration did not complete, "+
			"so this process refuses to start (C-SCHEMA-19). Inspect the schema, then clear the flag with an "+
			"explicit force to a known-good version — `migrate force %d` (golang-migrate CLI, same "+
			"schema_migrations table) — and restart; the server never clears it automatically",
		path, version, version-1)
}
