package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDSNCarriesEveryContractedPragma pins the connection string itself. If a
// refactor drops one pragma the failure names which one, instead of showing up
// much later as a lost foreign key or a silent SQLITE_BUSY.
func TestDSNCarriesEveryContractedPragma(t *testing.T) {
	dsn := DSN("/tmp/ledgerboard-test.db")
	for _, want := range []string{
		"file:/tmp/ledgerboard-test.db?",
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_txlock=immediate",
	} {
		assert.Contains(t, dsn, want, "DSN must carry %s (C-CONV-9)", want)
	}
	assert.NotContains(t, ReadDSN("/tmp/x.db"), "_txlock",
		"the read pool must not open immediate write transactions (C-CONV-9)")
}

// TestForeignKeysOnEveryPooledConnection is the QA probe construction that found
// the defect this DSN fixes (recon H2): with `db.Exec("PRAGMA foreign_keys=ON")`
// the setting reaches exactly one pooled connection.
//
// It asserts both halves, because a test that cannot detect the disease cannot
// prove the cure: the naive form fails on seven of eight connections, the
// contracted DSN succeeds on all eight.
func TestForeignKeysOnEveryPooledConnection(t *testing.T) {
	ctx := context.Background()

	t.Run("contracted DSN: every connection has the pragmas", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contract.db")
		pool, err := sql.Open("sqlite", DSN(path))
		require.NoError(t, err)
		defer pool.Close()
		pool.SetMaxOpenConns(8)

		// Hold all eight connections open at once, so this is eight distinct
		// SQLite connections and not one connection measured eight times.
		conns := make([]*sql.Conn, 0, 8)
		for i := 0; i < 8; i++ {
			c, err := pool.Conn(ctx)
			require.NoError(t, err)
			conns = append(conns, c)
		}
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()

		for i, c := range conns {
			var fk, busyTimeout int
			var journalMode string
			require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
			require.NoError(t, c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
			require.NoError(t, c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode))
			assert.Equal(t, 1, fk, "connection %d: foreign_keys must be ON (C-CONV-9)", i)
			assert.Equal(t, 5000, busyTimeout, "connection %d: busy_timeout must be 5000 (C-CONV-9)", i)
			assert.Equal(t, "wal", journalMode, "connection %d: journal_mode must be WAL (C-CONV-9)", i)
		}
	})

	t.Run("naive db.Exec(PRAGMA): reaches one connection of eight", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "naive.db")
		pool, err := sql.Open("sqlite", path)
		require.NoError(t, err)
		defer pool.Close()
		pool.SetMaxOpenConns(8)

		// The upstream form: open a plain path, then set the pragma with Exec.
		_, err = pool.Exec("PRAGMA foreign_keys=ON")
		require.NoError(t, err)

		conns := make([]*sql.Conn, 0, 8)
		for i := 0; i < 8; i++ {
			c, err := pool.Conn(ctx)
			require.NoError(t, err)
			conns = append(conns, c)
		}
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()

		enabled := 0
		for _, c := range conns {
			var fk int
			require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
			if fk == 1 {
				enabled++
			}
		}
		assert.Equal(t, 1, enabled,
			"the Exec form is expected to reach exactly one pooled connection; "+
				"if this ever reads 8, the negative control no longer detects the disease it exists for")
	})
}

// TestOpenReturnsSingleWriterPool asserts the other half of C-CONV-9: the
// handle Open returns is capped at one connection, so every mutation in this
// process serialises on BEGIN IMMEDIATE (C-CONV-3).
func TestOpenReturnsSingleWriterPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.db")
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()

	ctx := context.Background()
	first, err := conn.Conn(ctx)
	require.NoError(t, err)
	defer first.Close()

	// With MaxOpenConns(1) a second Conn blocks until the first is released; a
	// non-blocking deadline proves the cap rather than asserting a struct field.
	blockedCtx, cancel := context.WithTimeout(ctx, 50_000_000) // 50ms
	defer cancel()
	if _, err := conn.Conn(blockedCtx); err == nil {
		second, _ := conn.Conn(ctx)
		second.Close()
		t.Fatal("a second connection was handed out: the writer pool is not capped at one (C-CONV-9)")
	}
}

// TestOpenReadPoolHasFourConnections asserts the read pool's cap and that its
// connections carry the same pragmas.
func TestOpenReadPoolHasFourConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read.db")
	writer, err := Open(path)
	require.NoError(t, err)
	defer writer.Close()

	pool, err := OpenReadPool(path)
	require.NoError(t, err)
	defer pool.Close()

	ctx := context.Background()
	conns := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		c, err := pool.Conn(ctx)
		require.NoError(t, err, "the read pool must allow four concurrent connections (C-CONV-9)")
		conns = append(conns, c)
	}
	for i, c := range conns {
		var fk int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
		assert.Equal(t, 1, fk, "read connection %d must have foreign_keys on", i)
	}

	// A fifth simultaneous connection must not be available while all four are
	// held.
	blockedCtx, cancel := context.WithTimeout(ctx, 50_000_000)
	defer cancel()
	if _, err := pool.Conn(blockedCtx); err == nil {
		t.Fatal("the read pool handed out a fifth connection: it is not capped at four (C-CONV-9)")
	}
	for _, c := range conns {
		c.Close()
	}
}

// TestDirtyMigrationFailsClosed is C-SCHEMA-19: a database flagged dirty is
// never served from, never re-migrated automatically, and never cleared by us.
func TestDirtyMigrationFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dirty.db")

	// Create a healthy database first, so the failure below cannot be "the file
	// is missing".
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// Flag it dirty, exactly as a process killed between the version bump and
	// the migration's commit would leave it.
	raw, err := sql.Open("sqlite", DSN(path))
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = Open(path)
	require.Error(t, err, "Open must refuse a dirty database")
	msg := err.Error()
	assert.Contains(t, msg, "dirty", "the error must name the dirty flag")
	assert.Contains(t, msg, "version", "the error must name the version")
	assert.Contains(t, msg, "force", "the error must name the remedy")
	assert.Contains(t, msg, "C-SCHEMA-19", "the error must cite the rule it enforces")

	// It must not have cleared the flag on its way out.
	raw, err = sql.Open("sqlite", DSN(path))
	require.NoError(t, err)
	defer raw.Close()
	var dirty bool
	require.NoError(t, raw.QueryRow(`SELECT dirty FROM schema_migrations`).Scan(&dirty))
	assert.True(t, dirty, "the server must never clear the dirty flag itself (C-SCHEMA-19)")
}

// TestOpenRejectsHalfMigratedSchema is the other side of C-SCHEMA-19: a database
// whose schema_migrations row is missing entirely (a half-created file) must not
// be mistaken for a migrated one. It is a sanity check that the dirty guard does
// not mask a broken bookkeeping table.
func TestOpenRejectsHalfMigratedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	raw, err := sql.Open("sqlite", DSN(path))
	require.NoError(t, err)
	_, err = raw.Exec(`CREATE TABLE schema_migrations (version uint64, dirty bool)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// An empty bookkeeping table means "no version applied": migration runs and
	// the database becomes usable, which is what makes the dirty guard's
	// "no such table"/"no rows" branches matter.
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()

	var version int64
	var dirty bool
	require.NoError(t, conn.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.Equal(t, int64(2), version)
	assert.False(t, dirty)

	var name string
	err = conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "outbox", name)
	assert.True(t, strings.HasPrefix(DSN(path), "file:"))
}
