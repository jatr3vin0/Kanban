// Command harness-dbproc is the direct-DB test process of docs/test-plan.md
// §1.2 (build requirement H4): a real OS process that opens the shared SQLite
// file with the production DSN (C-CONV-9 — the test never "helps" with a
// different connection string) and executes one op.
//
// Ops: sql (query or exec), tx_hold (hold BEGIN IMMEDIATE for N ms — the
// busy-DB lock holder of T-CLAIM-16), burst (N sequential writes — the recon
// §4.5 negative control), wait_schema (tight-loop poll of PRAGMA table_info /
// sqlite_master / schema_migrations until a landmark is visible — the
// migration watcher of §4.3), dump (rowid-ordered JSON snapshot of tables),
// master (sqlite_master snapshot), force (the golang-migrate `force` semantics
// on the bookkeeping table — the operator recovery step of C-SCHEMA-19, run by
// a separate process exactly like the operator's CLI would), fk_check and
// migrations (bookkeeping read).
//
// Test support only: never imported by product code, never shipped.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rgracey/kanban-mcp/internal/db"
)

const (
	envRole     = "LEDGERBOARD_TEST_DBPROC" // must be "1" or the child refuses
	envOp       = "LEDGERBOARD_TEST_OP"
	envDB       = "LEDGERBOARD_TEST_DB"
	envOut      = "LEDGERBOARD_TEST_OUT" // result file for dumps
	envSQL      = "LEDGERBOARD_TEST_SQL"
	envSQLMode  = "LEDGERBOARD_TEST_SQLMODE" // "exec" (default) | "query"
	envBurstN   = "LEDGERBOARD_TEST_BURST_N"
	envHoldMS   = "LEDGERBOARD_TEST_HOLD_MS"
	envWaitKind = "LEDGERBOARD_TEST_WAIT_KIND" // column|table|dirty|dirty_with_table
	envWaitName = "LEDGERBOARD_TEST_WAIT_NAME" // table.column | table | version
	envWaitMs   = "LEDGERBOARD_TEST_WAIT_MAX_MS"
	envTables   = "LEDGERBOARD_TEST_TABLES" // comma list for dump
	envForceV   = "LEDGERBOARD_TEST_FORCE_VERSION"
)

type result struct {
	Op           string           `json:"op"`
	OK           bool             `json:"ok"`
	Err          string           `json:"err,omitempty"`
	RowsAffected int64            `json:"rows_affected,omitempty"`
	LastInsertID int64            `json:"last_insert_id,omitempty"`
	QueryRows    []map[string]any `json:"query_rows,omitempty"`
	HeldMS       int64            `json:"held_ms,omitempty"`
	WaitedMS     int64            `json:"waited_ms,omitempty"`
	SawLandmarks []string         `json:"saw_landmarks,omitempty"`
	Tables       map[string]any   `json:"tables,omitempty"`
	Version      *int64           `json:"version,omitempty"`
	Dirty        *bool            `json:"dirty,omitempty"`
	FKViolations []map[string]any `json:"fk_violations,omitempty"`
}

func getenv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-dbproc: %v\n", err)
		os.Exit(3)
	}
}

func openDB(path string) (*sql.DB, error) {
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

func run() error {
	if getenv(envRole) != "1" {
		return fmt.Errorf("%s must be 1; refusing to act as a stray process", envRole)
	}
	op := getenv(envOp)
	dbPath, err := require(envDB)
	if err != nil {
		return err
	}
	conn, err := openDB(dbPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer conn.Close()

	res := result{Op: op}
	switch op {
	case "sql":
		err = opSQL(conn, &res)
	case "execfile":
		err = opExecFile(conn, &res)
	case "tx_hold":
		err = opTxHold(conn, &res)
	case "burst":
		err = opBurst(conn, &res)
	case "wait_schema":
		err = opWaitSchema(conn, &res)
	case "dump":
		err = opDump(conn, &res)
	case "master":
		err = opMaster(conn, &res)
	case "force":
		err = opForce(conn, &res)
	case "fk_check":
		err = opFKCheck(conn, &res)
	case "migrations":
		err = opMigrations(conn, &res)
	default:
		return fmt.Errorf("unknown op %q", op)
	}
	if err != nil {
		res.Err = err.Error()
	}
	return writeOut(res)
}

func require(name string) (string, error) {
	v := getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func writeOut(res result) error {
	line, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fmt.Println(string(line))
	if p := getenv(envOut); p != "" {
		return os.WriteFile(p, append(line, '\n'), 0o600)
	}
	return nil
}

// opExecFile executes a semicolon-separated SQL file — the explicit Down
// migration of T-MIG-04, whose text is read from the repo's own migrations
// directory by the parent (no drift copy).
func opExecFile(conn *sql.DB, res *result) error {
	path, err := require("LEDGERBOARD_TEST_SQLFILE")
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(string(raw)); err != nil {
		return fmt.Errorf("execfile %s: %w", path, err)
	}
	res.OK = true
	return nil
}

func opSQL(conn *sql.DB, res *result) error {
	stmt, err := require(envSQL)
	if err != nil {
		return err
	}
	mode := getenv(envSQLMode)
	if mode == "query" {
		rows, err := conn.Query(stmt)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		out := []map[string]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			m := map[string]any{}
			for i, c := range cols {
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				m[c] = v
			}
			out = append(out, m)
		}
		res.QueryRows = out
		res.OK = rows.Err() == nil
		return rows.Err()
	}
	r, err := conn.Exec(stmt)
	if err != nil {
		return err
	}
	res.RowsAffected, _ = r.RowsAffected()
	res.LastInsertID, _ = r.LastInsertId()
	res.OK = true
	return nil
}

// opTxHold holds BEGIN IMMEDIATE for HOLD_MS and commits — the busy-DB lock
// holder. It reports "locked" on stdout the moment the transaction is open, so
// the parent can start the claimant without a sleep: the claimant's
// busy_timeout is what waits.
func opTxHold(conn *sql.DB, res *result) error {
	hold, err := strconv.ParseInt(getenv(envHoldMS), 10, 64)
	if err != nil || hold <= 0 {
		return fmt.Errorf("%s must be a positive ms", envHoldMS)
	}
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE boards SET description = description WHERE 0`); err != nil {
		tx.Rollback()
		return fmt.Errorf("take write lock: %w", err)
	}
	start := time.Now()
	fmt.Println(`{"op":"tx_hold","locked":true}`)
	time.Sleep(time.Duration(hold) * time.Millisecond)
	if err := tx.Commit(); err != nil {
		return err
	}
	res.HeldMS = time.Since(start).Milliseconds()
	res.OK = true
	return nil
}

// opBurst runs BURST_N sequential UPDATEs — the recon §4.5 negative control
// workload (4 processes × 10 writes must be 40/40 under the contract DSN).
func opBurst(conn *sql.DB, res *result) error {
	n, err := strconv.Atoi(getenv(envBurstN))
	if err != nil || n <= 0 {
		return fmt.Errorf("%s must be a positive int", envBurstN)
	}
	for i := 0; i < n; i++ {
		if _, err := conn.Exec(`UPDATE boards SET description = description WHERE 0`); err != nil {
			return fmt.Errorf("burst iteration %d: %w", i, err)
		}
		res.RowsAffected++
	}
	res.OK = true
	return nil
}

// opWaitSchema polls the schema in a tight loop until KIND/NAME is visible.
// Polling a local file-backed DB is not a sleep (§1.2). While waiting it
// records every OTHER landmark it observes along the way — the migration
// watcher evidence of §4.3/T-CRASH-d (which landmarks were reached, in which
// order).
func opWaitSchema(conn *sql.DB, res *result) error {
	kind, err := require(envWaitKind)
	if err != nil {
		return err
	}
	name, err := require(envWaitName)
	if err != nil {
		return err
	}
	maxMS := int64(30000)
	if v := getenv(envWaitMs); v != "" {
		if maxMS, err = strconv.ParseInt(v, 10, 64); err != nil {
			return err
		}
	}
	start := time.Now()
	seen := map[string]bool{}
	for {
		var (
			hasVersionCol, hasHandoffIDCol        bool
			handoffsTable, outboxTable, idemTable bool
			dirtySeen                             bool
		)
		_ = conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tickets') WHERE name='version'`).Scan(&hasVersionCol)
		_ = conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tickets') WHERE name='handoff_id'`).Scan(&hasHandoffIDCol)
		_ = conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='handoffs'`).Scan(&handoffsTable)
		_ = conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&outboxTable)
		_ = conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='idempotency_keys'`).Scan(&idemTable)
		var v int64
		if err := conn.QueryRow(`SELECT dirty FROM schema_migrations LIMIT 1`).Scan(&v); err == nil && v == 1 {
			dirtySeen = true
		}
		if hasVersionCol && !seen["tickets.version"] {
			seen["tickets.version"] = true
			res.SawLandmarks = append(res.SawLandmarks, "tickets.version")
		}
		if hasHandoffIDCol && !seen["tickets.handoff_id"] {
			seen["tickets.handoff_id"] = true
			res.SawLandmarks = append(res.SawLandmarks, "tickets.handoff_id")
		}
		if handoffsTable && !seen["table.handoffs"] {
			seen["table.handoffs"] = true
			res.SawLandmarks = append(res.SawLandmarks, "table.handoffs")
		}
		if outboxTable && !seen["table.outbox"] {
			seen["table.outbox"] = true
			res.SawLandmarks = append(res.SawLandmarks, "table.outbox")
		}
		if idemTable && !seen["table.idempotency_keys"] {
			seen["table.idempotency_keys"] = true
			res.SawLandmarks = append(res.SawLandmarks, "table.idempotency_keys")
		}
		if dirtySeen && !seen["dirty"] {
			seen["dirty"] = true
			res.SawLandmarks = append(res.SawLandmarks, "dirty")
		}
		done := false
		switch kind {
		case "column":
			done = seen["tickets."+name]
		case "table":
			done = seen["table."+name]
		case "dirty":
			done = dirtySeen
		case "dirty_with_table":
			// The second observable boundary: the migration's DDL is visible
			// while the dirty flag is still set.
			done = dirtySeen && seen["table."+name]
		default:
			return fmt.Errorf("unknown wait kind %q", kind)
		}
		if done {
			res.OK = true
			res.WaitedMS = time.Since(start).Milliseconds()
			return nil
		}
		if time.Since(start).Milliseconds() > maxMS {
			res.WaitedMS = time.Since(start).Milliseconds()
			return fmt.Errorf("landmark %s:%s not seen within %dms; saw %v", kind, name, maxMS, res.SawLandmarks)
		}
	}
}

// opDump writes a rowid-ordered JSON snapshot of the named (or all 0001)
// tables — the byte-comparison baseline of §4.1/§4.2.
func opDump(conn *sql.DB, res *result) error {
	tables := []string{}
	if t := getenv(envTables); t != "" {
		tables = strings.Split(t, ",")
	} else {
		rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('schema_migrations') ORDER BY name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, n)
		}
		rows.Close()
	}
	out := map[string]any{}
	for _, t := range tables {
		q := `SELECT * FROM ` + t + ` ORDER BY rowid`
		rows, err := conn.Query(q)
		if err != nil {
			return fmt.Errorf("dump %s: %w", t, err)
		}
		cols, _ := rows.Columns()
		table := []map[string]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			m := map[string]any{}
			for i, c := range cols {
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				m[c] = v
			}
			table = append(table, m)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		out[t] = table
	}
	res.Tables = out
	res.OK = true
	return nil
}

func opMaster(conn *sql.DB, res *result) error {
	rows, err := conn.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var typ, name, tbl string
		var sqlv sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &sqlv); err != nil {
			return err
		}
		list = append(list, map[string]any{"type": typ, "name": name, "tbl_name": tbl, "sql": sqlv.String})
	}
	res.QueryRows = list
	res.OK = rows.Err() == nil
	return rows.Err()
}

// opForce applies golang-migrate's `force V` semantics to the bookkeeping
// table — SetVersion(V, false) — with no migration executed. This is the
// operator recovery step C-SCHEMA-19 names; the product never does it itself,
// and this helper only exists because the CLI binary is not part of this repo.
func opForce(conn *sql.DB, res *result) error {
	v, err := strconv.ParseInt(getenv(envForceV), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", envForceV, err)
	}
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM schema_migrations`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (?, 0)`, v); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	res.OK = true
	res.Version = &v
	return nil
}

func opFKCheck(conn *sql.DB, res *result) error {
	rows, err := conn.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var table string
		var rowid, parent, fkidx int64
		if err := rows.Scan(&table, &rowid, &parent, &fkidx); err != nil {
			return err
		}
		list = append(list, map[string]any{"table": table, "rowid": rowid, "parent": parent, "fkid": fkidx})
	}
	res.FKViolations = list
	res.OK = rows.Err() == nil
	return rows.Err()
}

func opMigrations(conn *sql.DB, res *result) error {
	var version int64
	var dirty bool
	err := conn.QueryRow(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	switch {
	case err == nil:
		res.Version = &version
		res.Dirty = &dirty
		res.OK = true
	case err == sql.ErrNoRows:
		res.OK = true
	default:
		if strings.Contains(err.Error(), "no such table") {
			res.OK = true
			return nil
		}
		return err
	}
	return nil
}

var _ = io.EOF
