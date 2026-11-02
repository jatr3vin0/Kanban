# Finding A14 — T-MIG-05: the pinned upstream binary refuses a version-2 database by design (premise defect, ruling pending)

Status: **OPEN — premise ruling pending with the architect. Not a product defect; not fixed
in run 6.** The row is reported FAIL honestly in the run-6 index.

## What the row claims

`internal/mptest/migration_mp_test.go` (`TestMP_T_MIG_05`, docs/test-plan.md §3 row T-MIG-05):
"the upstream binary at the pinned commit opens the 0002 database — creates, updates and
lists all 2xx, upstream reads ignore the added columns, no silent corruption (dump-diff)."

## What actually happens (run 5 @ 47fb64a, reproduced in run 6)

The row migrates a copy of the upstream-populated fixture to version 2 with the product
runner (`migrateWithProduct`), then starts the pinned upstream binary against it. The
upstream binary fails at startup, before binding its listener:

```
{"level":"ERROR","msg":"failed to open database","error":"failed to run migrations: no migration found for version 2: read down for version 2 migrations: file does not exist"}
```

`waitForPort` therefore times out and the row fails at its `require.True` (migration_mp_test.go:722).

## Diagnosis — the failure is in the row's premise, not the schema

golang-migrate (v4.19.1, the migration runner inside BOTH binaries) reconciles the database's
`schema_migrations` row against the migration files embedded in the binary that opens it. The
pinned upstream binary's migration source stops at version 1 — the fork's `0002_lease`
migrations do not exist inside it. Handed a database stamped `version=2, dirty=0`, it looks
for the version-2 migration to validate against, finds neither up nor down file, and fails
closed (`C-SCHEMA-19` behaviour, correctly). The database itself is good: the product binary
opens and serves the same migrated file in `T-MIG-06`, and the version-1 data under it is
proven intact by the dump-diff legs of T-MIG-02/04.

So the premise "upstream binary opens the 0002 database" is false for **any** additive schema
version: an old binary whose migration set ends at version N cannot open a version N+1
database — that is golang-migrate failing closed, not corruption and not an additive-compat
violation in the schema.

## Options for the ruling (owner: architect)

1. **Reword the row's premise**: the compatibility claim the fork actually needs is "the
   upstream binary opens a version-1 database and reads it correctly" (its reads ignore the
   added columns — but a v1 database has none to ignore; the v2 database is proven by the
   product binary + dump-diff). The row then migrates nothing for the upstream leg.
2. **Ship `0002_lease.down.sql`** and extend the upstream fixture's migration source so the
   old binary can reconcile version 2. This is a schema-set change with contract implications
   (the additive-compatibility rule C-SCHEMA-4 speaks to adding migrations, not to what an
   older binary does at version N+1) — needs a contract note, not just a file.
3. **Drop the upstream-binary leg** from T-MIG-05 and fold its "upstream reads ignore added
   columns" observation into the dump-diff rows, keeping the pinned binary for a v1-only
   compatibility row.

## Evidence

- Run-5 transcript: `docs/test-results/phase-2/multiprocess.txt` (`=== RUN   TestMP_T_MIG_05`),
  upstream log captured in the failure message.
- Vendored source: the upstream binary's migration source ends at 0001; `golang-migrate/v4`
  `database/sqlite` fails closed on an unknown version (same fail-closed path T-CRASH-d
  exercises — `C-SCHEMA-19`).
- The row passed in no run: it was scheduled after the `T-MEM-01-CLAIMRACE` panic in run 4
  (binary aborted), and failed at this same require in run 5.

— concurrency-engineer, run 6 (2026-11-02)
