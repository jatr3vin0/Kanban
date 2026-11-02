# Finding A12 — T-CRASH-d kill model amended (B1 kill / B2 construct) — CORRECTED per the design review

Supersedes the original proposed text of this finding. The architect's design review (PR #7,
2026-11-02, §1) ACCEPTED the underlying discovery — per-statement landmarks do not exist for the
vendored runner — and REJECTED the original amendment text for three defects, corrected here.
Recorded by the concurrency engineer, 2026-11-02 (run 5 batch).

## The finding (unchanged, verified in the vendored source)

The test plan's §4.3 prescribed per-statement kill landmarks: a `harness-dbproc` watcher polls
`PRAGMA table_info(tickets)` until the k-th contracted statement of `0002_lease.up.sql` is visible
(k=1 `version`, k=8 `handoff_id`, k=11 `handoffs`, k=14 `outbox`, k=19 `idempotency_keys` — that
was §4.3's list), and the controller SIGKILLs the migrating server at that landmark. **These
per-statement landmarks do not exist at runtime**: the watcher observed every DDL landmark together
in one observation (`landmarks.json`, `saw_landmarks` = the single "dirty" boundary).

Cause — the vendored driver, `github.com/golang-migrate/migrate/v4 v4.19.1` (`go list -m`),
`database/sqlite/sqlite.go`:
- lines 189–200, `func (m *Sqlite) Run(migration io.Reader)`: reads the whole migration file and,
  unless `config.NoTxWrap`, hands the entire file to `executeQuery`;
- lines 202–217, `func (m *Sqlite) executeQuery(query string)`: `tx, err := m.db.Begin()` (203),
  a single `tx.Exec(query)` over the **whole file** (207), then `tx.Commit()` (213).

The whole 0002 file therefore commits as ONE transaction. SQLite DDL is transactional, so there is
no intermediate committed state for a per-statement watcher to observe. "Landmarks are
per-statement because each DDL statement commits individually" (§4.3) is factually wrong for this
driver — the contract's own DDL is fine; the *observation model* is what needed the amendment.

## Corrections demanded by the review, now applied

- **(a) B2 is not exercised by a watcher — so it is no longer claimed to be.** The row has exactly
  one kill point: B1, the dirty boundary (watcher observes `dirty` → controller SIGKILL). B2 is a
  *state*, not an event, and is CONSTRUCTED: a clean-migrated copy of the fixture gets
  `schema_migrations.dirty = 1` by direct SQL through `harness-dbproc`, and the identical
  fail-closed contract is asserted against it. The old sentence "kills the migrating server at
  (B1) … and (B2)" described a test that did not exist; it is gone.
- **(b) Per-boundary `force` targets.** B1 recovers with `force 1` (0002 has not applied); B2
  recovers with `force 2` (the DDL is already on disk — `force 1` there would re-run an applied
  file and fail on `duplicate column name` / `table already exists`). The old blanket "only the
  explicit operator force completes the migration" hid that the target version is
  boundary-dependent; §4.3 now states both targets.
- **(c) The two landmark lists are unified.** §4.3's list was
  `(k=1 version, k=8 handoff_id, k=11 handoffs, k=14 outbox, k=19 idempotency_keys)`; §3 row d's
  list was `k ∈ {1, 4, 8, 10, 16, 20}`. BOTH are superseded and both are now quoted against their
  own superseded sections in the single amended §4.3 — no list survives in an active rule. The
  20-statement count of 0002 remains only as the falsifiability bound for the throwaway-copy
  counterfactual.

## Amended A12 text (what §4.3 now says — matches the implemented row)

> **A12 (test plan §4.3, T-CRASH-d kill model — B1 kill / B2 construct).** The migration runner is
> vendored golang-migrate v4.19.1 (`database/sqlite/sqlite.go`: `Run` → `executeQuery`, lines
> 189–217): unless `NoTxWrap` is set it tx-wraps the ENTIRE migration file in one
> `Begin`/`Exec`/`Commit`, and SQLite DDL is transactional — so no per-statement DDL landmark is
> ever observable committed (empirical: the watcher sees all landmarks in one observation). The
> row verifies the two boundaries that DO exist: **B1** — the dirty boundary, as a kill (the row's
> only kill point): a separate `harness-dbproc` watcher observes `schema_migrations.dirty=1` from
> another process and the controller SIGKILLs the migrating server; and **B2** — the
> DDL-complete-with-dirty state, CONSTRUCTED, not raced: a clean-migrated copy gets
> `schema_migrations.dirty = 1` by direct SQL through `harness-dbproc` (the window is microseconds
> inside one process; an external poller cannot hit it reliably). The recovery contract is
> unchanged and applies at each boundary; the `force` target is boundary-dependent: **B1 →
> `force 1`** (0002 has not applied), **B2 → `force 2`** (the DDL is already on disk — `force 1`
> there would fail on `duplicate column name`). At each boundary: restart exits non-zero **before
> the listener binds** (C-SCHEMA-19, probe HTTP refused), the one-line message names the version
> and the remedy, and after the explicit operator force the end state is dump-identical to the
> clean migration of T-MIG-02, `foreign_key_check` is empty, and every pre-existing ticket keeps
> `version=1`, `lease_generation=0`. A server that half-serves with missing lease columns remains
> a test failure, not a recovery. The §4.3 per-statement landmark list (k=1/8/11/14/19) and the
> §3 row d list (k ∈ {1, 4, 8, 10, 16, 20}) are BOTH superseded; the 20-statement count of 0002
> remains the falsifiability bound for the throwaway-copy counterfactual. Deferred, recorded not
> dropped: per-statement landmarks return if a per-statement-committing runner is adopted
> (`NoTxWrap`), and are not currently constructible.

## Where the amendment landed (the review's seven locations)

| Location | Status |
|---|---|
| `test-plan.md` §4.3 (whole procedure) | Replaced with the B1-kill / B2-construct design above; fail-closed + force + dump-identical recovery paragraph kept verbatim; deferred-landmark note included. |
| `test-plan.md` §3 row d, "Exact kill point(s)" cell | `k ∈ {1, 4, 8, 10, 16, 20}` deleted → B1 kill + B2 constructed state; "the test records which boundary was reached" kept. |
| `test-plan.md` §3 honesty note | Replaced: the runner tx-wraps the file, so the observable boundary is the dirty window, not DDL steps. |
| `test-plan.md` §1.4 | Watcher-landmark phrase → "the dirty-boundary kill (B1) and constructed dirty state (B2), §4.3". |
| `test-plan.md` §2.7 helper row (`wait_column`) | Reference re-pointed at the amended §4.3; helper stays (B1 uses it on `dirty`). |
| `test-plan.md` §4.3 recovery sentence | Per-boundary target stated: B1 → force 1; B2 → force 2. |
| `T-CRASH-d/verdict.txt` | No longer written unconditionally — derived from `t.Failed()`; claims dump-identical only when the dump comparisons actually ran and passed. |

Implementation: `internal/mptest/migration_mp_test.go` `TestMP_T_CRASH_D` (B1 leg unchanged in
substance; B2 leg added with `force 2`), evidence `recovery.json` records both boundaries and the
boundary rule.
