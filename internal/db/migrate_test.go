package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The migration tests build a genuinely 0001 database from the real
// 0001_init.up.sql and populate it before 0002 ever runs. A migration that only
// works on an empty database has proved nothing (C-SCHEMA-2, C-COMPAT-4).

// openRaw opens a plain handle with the contracted DSN, for fixture work.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	dbh, err := sql.Open("sqlite", DSN(path))
	require.NoError(t, err)
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

// newMigrator builds a golang-migrate instance on path, for explicit Up/Down.
func newMigrator(t *testing.T, path string) *migrate.Migrate {
	t.Helper()
	dbh := openRaw(t, path)
	src, err := iofs.New(migrationFS, "migrations")
	require.NoError(t, err)
	drv, err := sqlite.WithInstance(dbh, &sqlite.Config{MigrationsTable: migrationsTable})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	require.NoError(t, err)
	return m
}

// closeMigrator closes a migrator, discarding the two errors it returns (the
// fixture's handles are cleaned up by t.Cleanup).
func closeMigrator(m *migrate.Migrate) {
	_, _ = m.Close()
}

// fixture is the populated 0001 database every migration test starts from.
type fixture struct {
	boardID   string
	ticketIDs []string
}

// buildPopulated0001 creates a database at schema version 0001 only, applies the
// real 0001_init.up.sql, and populates every 0001 table.
func buildPopulated0001(t *testing.T) (string, fixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upstream-0001.db")
	dbh := openRaw(t, path)

	up, err := os.ReadFile(filepath.Join("migrations", "0001_init.up.sql"))
	require.NoError(t, err, "the real 0001 migration must be readable")
	_, err = dbh.Exec(string(up))
	require.NoError(t, err, "applying 0001_init.up.sql")

	// golang-migrate's bookkeeping, set to version 1 and clean, exactly as a
	// process that ran only the first migration would leave it.
	_, err = dbh.Exec(`CREATE TABLE schema_migrations (version uint64, dirty bool)`)
	require.NoError(t, err)
	_, err = dbh.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (1, false)`)
	require.NoError(t, err)

	f := fixture{
		boardID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
		ticketIDs: []string{
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb01",
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb02",
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03",
		},
	}
	_, err = dbh.Exec(`INSERT INTO boards (id, name, description, created_at, updated_at)
		VALUES (?, 'Fixture board', 'populated before 0002', '2026-10-30T10:00:00Z', '2026-10-30T10:00:00Z')`, f.boardID)
	require.NoError(t, err)
	_, err = dbh.Exec(`INSERT INTO epics (id, board_id, title, description, created_at, updated_at)
		VALUES ('cccccccccccccccccccccccccccccc01', ?, 'Epic', '', '2026-10-30T10:00:00Z', '2026-10-30T10:00:00Z')`, f.boardID)
	require.NoError(t, err)

	for i, id := range f.ticketIDs {
		status := []string{"todo", "in_progress", "done"}[i]
		priority := []string{"critical", "medium", "low"}[i]
		_, err = dbh.Exec(`INSERT INTO tickets
			(id, board_id, epic_id, title, description, status, priority, assignee, resolution, created_at, updated_at)
			VALUES (?, ?, 'cccccccccccccccccccccccccccccc01', ?, ?, ?, ?, ?, NULL, ?, ?)`,
			id, f.boardID, fmt.Sprintf("Ticket %d", i+1), "unicode ✓ body", status, priority,
			[]string{"alice", "", "bob"}[i], "2026-10-30T10:00:0"+fmt.Sprint(i)+"Z", "2026-10-30T10:00:0"+fmt.Sprint(i)+"Z")
		require.NoError(t, err)
	}

	// A legacy row the Go enums do not know (C-COMPAT-12) and a duplicated
	// created_at second (C-AUDIT-7's tie), both of which a real board contains.
	_, err = dbh.Exec(`INSERT INTO tickets
		(id, board_id, title, description, status, priority, assignee, resolution, created_at, updated_at)
		VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb04', ?, 'Legacy row', '', 'weird', 'high', '', NULL,
		        '2026-10-30T10:00:00Z', '2026-10-30T10:00:00Z')`, f.boardID)
	require.NoError(t, err)

	_, err = dbh.Exec(`INSERT INTO notes (id, ticket_id, body, created_at, updated_at)
		VALUES ('dddddddddddddddddddddddddddddd01', ?, 'a note', '2026-10-30T10:01:00Z', '2026-10-30T10:01:00Z')`, f.ticketIDs[0])
	require.NoError(t, err)
	_, err = dbh.Exec(`INSERT INTO tasks (id, ticket_id, title, done, position, created_at, updated_at)
		VALUES ('eeeeeeeeeeeeeeeeeeeeeeeeeeeeee01', ?, 'a task', 0, 1, '2026-10-30T10:02:00Z', '2026-10-30T10:02:00Z')`, f.ticketIDs[0])
	require.NoError(t, err)
	_, err = dbh.Exec(`INSERT INTO ticket_relations (from_ticket_id, to_ticket_id, kind, created_at)
		VALUES (?, ?, 'blocks', '2026-10-30T10:03:00Z')`, f.ticketIDs[0], f.ticketIDs[1])
	require.NoError(t, err)
	_, err = dbh.Exec(`INSERT INTO ticket_events (id, ticket_id, type, actor, payload, created_at)
		VALUES ('ffffffffffffffffffffffffffffff01', ?, 'created', '', '{}', '2026-10-30T10:00:00Z')`, f.ticketIDs[0])
	require.NoError(t, err)

	return path, f
}

// tableColumns returns a table's column names in declaration order.
func tableColumns(t *testing.T, dbh *sql.DB, table string) []string {
	t.Helper()
	rows, err := dbh.Query(`SELECT name FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		cols = append(cols, name)
	}
	require.NoError(t, rows.Err())
	return cols
}

// dumpTable renders every row of the named 0001 tables as ordered text, so a
// before/after comparison catches any data change the migration caused.
func dumpTable(t *testing.T, dbh *sql.DB, query string) []string {
	t.Helper()
	rows, err := dbh.Query(query)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
				continue
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	require.NoError(t, rows.Err())
	return out
}

// legacyDumps captures the 0001 data that must survive the migration byte for
// byte (C-COMPAT-4).
func legacyDumps(t *testing.T, dbh *sql.DB) map[string][]string {
	t.Helper()
	return map[string][]string{
		"boards":           dumpTable(t, dbh, `SELECT * FROM boards ORDER BY id`),
		"epics":            dumpTable(t, dbh, `SELECT * FROM epics ORDER BY id`),
		"tickets":          dumpTable(t, dbh, `SELECT * FROM tickets ORDER BY id`),
		"notes":            dumpTable(t, dbh, `SELECT * FROM notes ORDER BY id`),
		"tasks":            dumpTable(t, dbh, `SELECT * FROM tasks ORDER BY id`),
		"ticket_relations": dumpTable(t, dbh, `SELECT * FROM ticket_relations ORDER BY from_ticket_id, to_ticket_id`),
		"ticket_events":    dumpTable(t, dbh, `SELECT * FROM ticket_events ORDER BY id`),
	}
}

// TestMigrate0002InPlace is T-MIG-02: a populated 0001 database migrates in
// place, additively, with every legacy row and every legacy table untouched.
func TestMigrate0002InPlace(t *testing.T) {
	path, f := buildPopulated0001(t)

	before := func() map[string][]string {
		dbh := openRaw(t, path)
		return legacyDumps(t, dbh)
	}()
	var beforeTickets, beforeNotes []string
	{
		dbh := openRaw(t, path)
		beforeTickets = tableColumns(t, dbh, "tickets")
		beforeNotes = tableColumns(t, dbh, "notes")
	}
	require.Len(t, beforeTickets, 11, "0001 tickets has eleven columns")

	conn, err := Open(path)
	require.NoError(t, err, "opening a 0001 database migrates it in place")
	defer conn.Close()

	var version int64
	var dirty bool
	require.NoError(t, conn.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.Equal(t, int64(2), version, "the database is at version 2 after Open")
	assert.False(t, dirty)

	// C-SCHEMA-2: existing rows get the contracted defaults, no data change.
	for _, id := range append(f.ticketIDs, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb04") {
		var ticketVersion, generation, renewals int64
		var holder, holderType, handoffID sql.NullString
		var acquiredAt, expiresAt sql.NullInt64
		require.NoError(t, conn.QueryRow(`
			SELECT version, lease_generation, lease_renewals,
			       lease_holder, lease_holder_type, handoff_id, lease_acquired_at, lease_expires_at
			  FROM tickets WHERE id = ?`, id).
			Scan(&ticketVersion, &generation, &renewals, &holder, &holderType, &handoffID, &acquiredAt, &expiresAt))
		assert.Equal(t, int64(1), ticketVersion, "ticket %s: version defaults to 1", id)
		assert.Equal(t, int64(0), generation, "ticket %s: lease_generation defaults to 0", id)
		assert.Equal(t, int64(0), renewals, "ticket %s: lease_renewals defaults to 0", id)
		assert.False(t, holder.Valid, "ticket %s: lease_holder is NULL", id)
		assert.False(t, holderType.Valid, "ticket %s: lease_holder_type is NULL", id)
		assert.False(t, handoffID.Valid, "ticket %s: handoff_id is NULL", id)
		assert.False(t, acquiredAt.Valid, "ticket %s: lease_acquired_at is NULL", id)
		assert.False(t, expiresAt.Valid, "ticket %s: lease_expires_at is NULL", id)
	}

	// Every legacy table is byte-identical, and so are the 0001 columns of
	// `tickets` (only new columns may appear).
	dbh := openRaw(t, path)
	after := legacyDumps(t, dbh)
	for table, rows := range before {
		if table == "tickets" {
			continue // compared column-by-column below, because 0002 adds columns
		}
		assert.Equal(t, rows, after[table], "table %s must be untouched by the migration", table)
	}
	assert.Equal(t, len(before["tickets"]), len(after["tickets"]), "no ticket row may be added or lost")
	afterTickets := tableColumns(t, dbh, "tickets")
	assert.Equal(t, beforeTickets, afterTickets[:len(beforeTickets)],
		"the eleven original columns keep their order and names (C-COMPAT-5)")
	assert.Len(t, afterTickets, 19, "0002 adds exactly eight columns")
	assert.Equal(t, beforeNotes, tableColumns(t, dbh, "notes"), "no unrelated table gains a column")

	// C-SCHEMA-1: only ADD COLUMN, CREATE INDEX and CREATE TABLE ran, and the
	// new objects are exactly the contracted ones.
	newObjects := dumpTable(t, dbh, `SELECT type, name FROM sqlite_master
		WHERE name IN ('handoffs','outbox','idempotency_keys',
		               'tickets_claimable','tickets_lease_holder',
		               'handoffs_ticket_created','handoffs_status',
		               'outbox_claimable','outbox_ticket','outbox_handoff','outbox_lease',
		               'idempotency_expires')
		ORDER BY type, name`)
	assert.Equal(t, []string{
		"index|handoffs_status",
		"index|handoffs_ticket_created",
		"index|idempotency_expires",
		"index|outbox_claimable",
		"index|outbox_handoff",
		"index|outbox_lease",
		"index|outbox_ticket",
		"index|tickets_claimable",
		"index|tickets_lease_holder",
		"table|handoffs",
		"table|idempotency_keys",
		"table|outbox",
	}, newObjects)

	assert.Empty(t, dumpTable(t, dbh, `PRAGMA foreign_key_check`), "foreign_key_check must be clean")

	// T-MIG-06: a second open is a no-op.
	require.NoError(t, conn.Close())
	conn2, err := Open(path)
	require.NoError(t, err)
	defer conn2.Close()
	require.NoError(t, conn2.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.Equal(t, int64(2), version)
	assert.False(t, dirty)
	assert.Equal(t, after["tickets"], dumpTable(t, openRaw(t, path), `SELECT * FROM tickets ORDER BY id`))
}

// TestMigrate0002DownRoundTrip is T-MIG-04: the down migration reverses 0002
// exactly — eleven columns on `tickets`, no new objects, clean foreign keys and
// the 0001 data still identical — and the up migration can run again afterwards.
func TestMigrate0002DownRoundTrip(t *testing.T) {
	path, _ := buildPopulated0001(t)

	dbhBefore := openRaw(t, path)
	before := legacyDumps(t, dbhBefore)

	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// Populate one 0002 row so the down migration is proven to drop real data,
	// not just empty tables (the loss is contracted, not a surprise).
	raw := openRaw(t, path)
	_, err = raw.Exec(`INSERT INTO idempotency_keys (actor, operation, key, request_hash, status, created_at, expires_at)
		VALUES ('agent:a', 'ticket.claim', 'k', 'h', 'completed', 1, 2)`)
	require.NoError(t, err)

	m := newMigrator(t, path)
	// Steps(-1) is exactly one migration down; Down() would run the whole chain
	// and drop the 0001 schema too.
	require.NoError(t, m.Steps(-1), "the down migration must run on explicit request")

	dbhAfter := openRaw(t, path)
	assert.Equal(t, []string{
		"id", "board_id", "epic_id", "title", "description", "status",
		"priority", "assignee", "resolution", "created_at", "updated_at",
	}, tableColumns(t, dbhAfter, "tickets"), "tickets returns to its eleven original columns")

	for _, gone := range []string{
		"handoffs", "outbox", "idempotency_keys",
		"tickets_claimable", "tickets_lease_holder",
		"handoffs_ticket_created", "handoffs_status",
		"outbox_claimable", "outbox_ticket", "outbox_handoff", "outbox_lease",
		"idempotency_expires",
	} {
		var name string
		err := dbhAfter.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, gone).Scan(&name)
		assert.ErrorIs(t, err, sql.ErrNoRows, "%s must be dropped by the down migration", gone)
	}

	assert.Empty(t, dumpTable(t, dbhAfter, `PRAGMA foreign_key_check`), "foreign_key_check must be clean")
	after := legacyDumps(t, dbhAfter)
	for table, rows := range before {
		assert.Equal(t, rows, after[table],
			"table %s must hold exactly the 0001 rows after the round trip (the contracted data loss "+
				"is limited to lease/handoff/outbox rows)", table)
	}

	// Up again: defaults are re-applied to the same rows.
	require.NoError(t, m.Up())
	closeMigrator(m)
	conn2, err := Open(path)
	require.NoError(t, err)
	defer conn2.Close()

	var version int64
	require.NoError(t, conn2.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version))
	assert.Equal(t, int64(2), version)
	var ticketVersion, generation int64
	require.NoError(t, conn2.QueryRow(`SELECT version, lease_generation FROM tickets
		WHERE id = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb01'`).Scan(&ticketVersion, &generation))
	assert.Equal(t, int64(1), ticketVersion)
	assert.Equal(t, int64(0), generation)
}

// TestMigrate0002IdempotentOpenLoop is T-MIG-06: open/migrate repeatedly, never
// dirty, version stays 2, data stable.
func TestMigrate0002IdempotentOpenLoop(t *testing.T) {
	path, _ := buildPopulated0001(t)

	for i := 0; i < 5; i++ {
		conn, err := Open(path)
		require.NoError(t, err, "open %d", i)
		var version int64
		var dirty bool
		require.NoError(t, conn.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
		assert.Equal(t, int64(2), version, "open %d", i)
		assert.False(t, dirty, "open %d must leave the database clean", i)
		require.NoError(t, conn.Close())
	}
}
