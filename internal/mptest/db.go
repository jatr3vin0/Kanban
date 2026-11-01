// Parent-side database access for the MP rows: every helper opens its own
// connection with the production DSN (C-CONV-9) — the parent is one more
// process on the file, never a shared handle.
package mptest

import (
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/rgracey/kanban-mcp/internal/db"
	"github.com/rgracey/kanban-mcp/internal/store"
	_ "modernc.org/sqlite"
)

type sqlDB struct{ db *sql.DB }

func openDSN(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", db.DSN(path))
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Int runs a single-value query.
func (d *sqlDB) Int(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("Int %q: %v", query, err)
	}
	return n
}

// Str runs a single-value query returning a string (empty when NULL).
func (d *sqlDB) Str(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := d.db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("Str %q: %v", query, err)
	}
	return s.String
}

// IntOrNull returns (value, true) or (0, false) when NULL.
func (d *sqlDB) IntOrNull(t *testing.T, query string, args ...any) (int64, bool) {
	t.Helper()
	var n sql.NullInt64
	if err := d.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("IntOrNull %q: %v", query, err)
	}
	return n.Int64, n.Valid
}

// Exists reports whether any row satisfies the query.
func (d *sqlDB) Exists(t *testing.T, query string, args ...any) bool {
	t.Helper()
	return d.Int(t, `SELECT COUNT(*) FROM (`+query+`)`, args...) > 0
}

// Strs runs a single-column query returning every row's string.
func (d *sqlDB) strs(t *testing.T, query string, args ...any) []string {
	t.Helper()
	rows, err := d.db.Query(query, args...)
	if err != nil {
		t.Fatalf("strs %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Exec runs a mutation and returns RowsAffected.
func (d *sqlDB) Exec(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	r, err := d.db.Exec(query, args...)
	if err != nil {
		t.Fatalf("Exec %q: %v", query, err)
	}
	n, _ := r.RowsAffected()
	return n
}

// Raw exposes the handle for one-off specialised queries (dumps, fk_check).
func (d *sqlDB) Raw() *sql.DB { return d.db }

// OpenStore opens a parent-side store with a file-backed clock (the same
// clocksource seam the children and the server binary read), so parent and
// children agree on now_ms without any shared handle.
func OpenStore(t *testing.T, path, clockFile string) *store.SQLiteStore {
	t.Helper()
	conn, err := db.Open(path)
	if err != nil {
		t.Fatalf("open store db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	clock, err := clockFromEnvOrFile(clockFile)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	return store.NewSQLiteStore(conn, store.WithClock(clock))
}

func dialPort(port int) (net.Conn, error) {
	d := net.Dialer{Timeout: 250 * time.Millisecond}
	return d.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}
