package db

import (
	"database/sql"
	"fmt"
	"net/url"
)

// Contract values for the connection setup (docs/contracts.md C-CONV-9).
//
// Every pragma is expressed as a DSN parameter rather than a post-open
// `db.Exec("PRAGMA ...")`. That is the whole point of the fix: `database/sql`
// pools connections, and a PRAGMA executed once lands on exactly one pooled
// connection. Recon/QA measured foreign_keys = 1 on 1 of 8 connections with the
// old form. modernc.org/sqlite applies `_pragma=` parameters to *every* new
// connection it opens, so the setting cannot be lost by pool growth.
const (
	// pragmaQuery is appended to every DSN we build. Order is fixed so the DSN
	// is byte-comparable in tests and logs.
	pragmaQuery = "_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	// txLockImmediate makes `db.BeginTx` issue BEGIN IMMEDIATE, which takes the
	// write lock at transaction start instead of on the first write. A deferred
	// transaction that upgrades mid-way can lose a lock race as SQLITE_BUSY
	// even with busy_timeout set; immediate cannot (C-CONV-3, C-ERR-7).
	txLockImmediate = "&_txlock=immediate"
)

// DSN returns the contracted connection string for a SQLite database file
// (C-CONV-9): pragmas that apply to every pooled connection, plus
// `_txlock=immediate` so every transaction this pool opens is a write
// transaction that takes the lock up front.
func DSN(path string) string {
	return "file:" + escapePath(path) + "?" + pragmaQuery + txLockImmediate
}

// ReadDSN is the same connection string without `_txlock=immediate`: it is used
// for the read pool, which never opens a write transaction and must not hold
// the database write lock (C-CONV-9's separate read pool).
func ReadDSN(path string) string {
	return "file:" + escapePath(path) + "?" + pragmaQuery
}

// escapePath percent-encodes the parts of a filesystem path that would
// otherwise be read as URI structure by the SQLite driver. A plain path is
// unchanged, so the common case stays readable in logs.
func escapePath(path string) string {
	u := url.URL{Path: path}
	return u.EscapedPath()
}

// OpenReadPool opens the contract's read pool: a separate *sql.DB with at most
// four connections (C-CONV-9). It carries the same per-connection pragmas as
// the writer but no `_txlock`, and it is never used for a mutation — the single
// writer connection is the only one that may take the write lock (C-CONV-3).
func OpenReadPool(path string) (*sql.DB, error) {
	pool, err := sql.Open("sqlite", ReadDSN(path))
	if err != nil {
		return nil, fmt.Errorf("failed to open read pool: %w", err)
	}
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(4)
	if err := pool.Ping(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to open read pool: %w", err)
	}
	return pool, nil
}
