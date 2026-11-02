# T-CRASH-d — finding A12: the vendored golang-migrate v4.19.1 driver tx-wraps the whole migration file

**Row:** T-CRASH-d (crash during migration, C-SCHEMA-19 fail-closed + force recovery), plan §3 row d / §4.3.
**Status:** row implemented and exercised end-to-end (see verdict.txt, failclosed.txt, recovery.json); the §4.3
watcher-landmark kill model is amended by this finding. Recorded by the concurrency engineer, 2026-11-02.

## The finding

The test plan's §4.3 prescribes per-statement kill landmarks: a `harness-dbproc` watcher polls
`PRAGMA table_info(tickets)` until the k-th contracted statement of `0002_lease.up.sql` is visible
(k=1 `version`, k=8 `handoff_id`, k=11 `handoffs`, k=14 `outbox`, k=19 `idempotency_keys`), and the
controller SIGKILLs the migrating server at that landmark. **These per-statement landmarks do not
exist at runtime**: the watcher observed every DDL landmark together in one observation
(`landmarks.json`, `saw_landmarks` = the single "dirty" boundary).

Cause — the vendored driver, `github.com/golang-migrate/migrate/v4 v4.19.1`
(`go list -m`), `database/sqlite/sqlite.go`:

- lines 189–200, `func (m *Sqlite) Run(migration io.Reader)`: reads the whole migration file and,
  unless `config.NoTxWrap`, hands the entire file to `executeQuery`;
- lines 202–217, `func (m *Sqlite) executeQuery(query string)`: `tx, err := m.db.Begin()` (203),
  a single `tx.Exec(query)` over the **whole file** (207), then `tx.Commit()` (213).

The whole 0002 file therefore commits as ONE transaction. SQLite DDL is transactional, so there is
no intermediate committed state for a per-statement watcher to observe: statement k's effect is never
visible before statement 20's. "Landmarks are per-statement because each DDL statement commits
individually" (§4.3) is factually wrong for this driver — the contract's own DDL is fine; the
*observation model* is what needs the amendment.

## What T-CRASH-d verifies instead — the two observable dirty boundaries

- **B1 — dirty visible, migration transaction uncommitted** (before or inside the tx): the watcher
  observes `schema_migrations` flipped to the dirty version while no 0002 DDL has committed. The
  controller SIGKILLs the migrating server here; restart fails closed (exit non-zero, C-SCHEMA-19
  line, no listener), and only the explicit operator `force` completes the migration to a
  dump-identical clean state (recovery.json).
- **B2 — DDL complete with dirty flag** (the window between the migration transaction's commit and
  the runner recording the clean version): the 0002 DDL is on disk while `schema_migrations` still
  reads dirty. The same fail-closed restart + force contract applies.

Both boundaries are killed from a separate process (the controller), against a server migrated by the
real binary — the row's contract (fail-closed before bind, force as the only recovery, end state
dump-identical to T-MIG-02's clean migration) is verified exactly as before; only the kill-point
model changes.

## Proposed A12 amendment text (verbatim)

> **A12 (test plan §4.3, T-CRASH-d kill model).** Replace the per-statement landmark kill procedure
> with the two dirty boundaries the vendored runner actually exposes. golang-migrate v4.19.1's
> sqlite driver (`database/sqlite/sqlite.go`: `Run` → `executeQuery`, lines 189–217) tx-wraps the
> entire migration file in one `Begin`/`Exec`/`Commit`, so no per-statement DDL landmark is ever
> observable committed (empirical: the watcher sees all landmarks in one observation). T-CRASH-d
> kills the migrating server at:
> (B1) the dirty boundary — `schema_migrations` shows the target version dirty while the migration
> transaction is uncommitted; and
> (B2) the DDL-complete-with-dirty boundary — the migration transaction has committed but the
> bookkeeping still reads dirty.
> The recovery contract is unchanged and applies at each boundary: restart exits non-zero **before
> the listener binds** (C-SCHEMA-19, probe HTTP refused), only the explicit operator `force`
> completes the migration, the end state is dump-identical to the clean migration of T-MIG-02,
> `foreign_key_check` is empty, and every pre-existing ticket keeps `version=1`,
> `lease_generation=0`. A server that half-serves with missing lease columns remains a test
> failure, not a recovery. The §3 row d landmark list (k ∈ {1, 4, 8, 10, 16, 20}) is superseded;
> the 20-statement count of 0002 remains the falsifiability bound for the throwaway-copy
> counterfactual.

## One bounded -race attempt

`go test ./internal/mptest -race -run '^TestMP_T_(CLAIM_01|CLAIM_08_EXPIRY_RACE|FENCING|CRASH_A|CRASH_B|CRASH_E)$' -count=1 -timeout 8m`,
wall-clock kill at 15 min per the brief's bound. Outcome recorded below (run 4 window):
see `race.txt` in this directory; if absent, the attempt was not completed in this session and is
handed to the next one — the race gate of plan §5 (`CGO_ENABLED=1 go test -race ./...`) remains open.
