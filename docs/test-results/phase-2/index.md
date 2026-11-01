# Phase 2, slice 1 — domain foundation: evidence

Branch `phase-2/domain-core`. Store-level only: no REST or MCP handler is part of this slice.

## What was run

`docs/test-results/phase-2/command.txt` lists the exact commands; the transcripts beside it are
verbatim output.

| File | Contents |
|---|---|
| `output.txt` | `go test ./... -count=1 -v` — whole suite, 69 passing tests |
| `db-and-migration.txt` | connection-pragma probe, dirty-migration fail-closed, 0002 up/down round trip on a populated 0001 database |
| `claims.txt` | claim / claim_next / renew / release store tests |
| `seams.txt` | crashpoint and clocksource seam tests |
| `review-fixes.txt` | the architect's FIX-FIRST follow-up: R1/R2/R3 verification, the two new tests, and both negative controls |

## Results

- `gofmt -l .` — clean.
- `go build ./...`, `go vet ./...` — clean.
- `go test ./... -count=1` — all packages `ok`; the 31 upstream tests still pass unchanged.

## Rule coverage in this slice

| Rule | Test |
|---|---|
| C-CONV-9 per-connection pragmas | `TestForeignKeysOnEveryPooledConnection` (8 held connections, `foreign_keys`=1 on all; the naive `db.Exec("PRAGMA …")` form is asserted to reach exactly 1 of 8 — the negative control) |
| C-CONV-9 single writer / read pool | `TestOpenReturnsSingleWriterPool`, `TestOpenReadPoolHasFourConnections` |
| C-SCHEMA-2/4 additive 0002 | `TestMigrate0002InPlace` (populated 0001 fixture built from the real `0001_init.up.sql`) |
| C-SCHEMA-4 down reverses exactly | `TestMigrate0002DownRoundTrip` (11 columns back, 12 objects dropped, `foreign_key_check` clean, all 0001 rows byte-identical) |
| C-SCHEMA-19 dirty fails closed | `TestDirtyMigrationFailsClosed` (error names the version and the remedy; the flag is never cleared by us) |
| C-INV-1 one holder | `TestClaimEightConcurrentClaimantsOneWinner` (8 separate connections → exactly 1 winner, 1 audit row) |
| C-CLAIM-2/12 claim_next | `TestClaimNextRaceGivesDistinctTickets`, `TestClaimNextPriorityOrder` |
| C-CLAIM-7 lapsed lease claimable | `TestClaimLapsedLeaseIsClaimable` |
| C-CLAIM-11 humans cannot claim | `TestClaimHumanForbidden` |
| C-CLAIM-14/15 renew | `TestRenewHeartbeat`, `TestRenewNeverResurrectsLapsedLease` |
| C-CLAIM-18 release has no clock | `TestReleaseDiagnosis/lapsed holder with its current generation succeeds` |
| C-INV-3 release clears every lease field | `TestReleaseClearsLeaseInOneRow` |
| C-CLAIM-17 generation monotonic | `TestGenerationIsMonotonic` (`1,1,2,3`) |
| §2.7 diagnosis order | `TestClaimDiagnosisOrder`, `TestRenewDiagnosisOrder`, `TestReleaseDiagnosis` |
| C-CONV-8 wire timestamps | `TestClaimJSONShape` |
| C-CONV-10 knobs | `TestClaimSortsLeaseTTL` |
| C-CONV-11 seams inert by default | `TestUnarmedIsInert`, `TestUnarmedSubprocessSurvives`, `TestArmedPointKillsOwnProcess` (SIGKILL, marker line in the FIFO) |

## Known gaps, deliberately left to later slices

- No sweeper loop: expiry is evaluated at claim time only (`C-CLAIM-20` is slice 4).
- No handoff, outbox, idempotency-store or memory-adapter code — those are slices 2 and 3; only the
  tables exist.
- The fake clock substitutes `now_ms` for the new claim paths only; upstream `created_at`/`updated_at`
  TEXT columns still come from `time.Now()`. See the PR notes for why (additive-only constraint).
