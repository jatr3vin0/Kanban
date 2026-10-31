# Ledgerboard — Test plan

**Status:** Phase 1 deliverable 2. A plan, not code — this document specifies every test; the tests
themselves are built and run in Phases 2–7. Not owner-ratified.
**Authority:** the owner's engineering spec §21 (testing) is the requirement; its areas are quoted
verbatim in §7 (coverage map). Rule IDs (`C-*`) are cited from
[`docs/contracts.md`](contracts.md) and [`docs/concurrency.md`](concurrency.md) (branch
`phase-1/contracts`, PR #2 — the IDs are stable by that document's own convention). Implementation
facts come from [`docs/recon.md`](recon.md). Where this plan and contracts.md disagree, contracts.md
wins and the disagreement is listed in §9.
**Scope guard:** this PR adds **only this file**. No test code, no product code, no migration files.

---

## 0. How to read this plan

Every test in §2 is one row with these fields — exactly the six the deliverable requires:

| Field | Column | Meaning |
|---|---|---|
| ID + name | **ID** | `T-<AREA>-<nn>`. Areas: REG regression, CLAIM ownership, IDEM idempotency, HAND handoff, MEM memory/outbox, AUTH authz, ERR errors, AUDIT audit, CRASH crash boundaries, MIG migration, E2E end-to-end. IDs are stable once merged; a renamed test is a plan revision. |
| Rules proved | **Rules** | The contract rule IDs the assertions prove. A row with no rule ID is not a test and must not exist. |
| What must be true | **Asserts** | Stated so it can fail: concrete expected status codes, cardinalities, field values. "X works" is banned. |
| How it runs | **Level** | `U` unit (in-process against the store, temp DB, one connection) · `I` integration (one real server process on a loopback port, real HTTP/MCP clients) · `MP` multi-process (the §1 harness: separate OS processes, separate connections) · `E2E` (§2.9: full fleet scenario). The level is chosen by the rule: anything whose failure mode is *concurrency* or *crash* must be `MP` — goroutines sharing one connection cannot reproduce `SQLITE_BUSY`, lost leases or a killed process, which is exactly what recon §4.5/§6 showed. |
| Determinism | **Det.** | How the test avoids flakiness: fake clock, barrier/handshake, bounded retry, seeded sequence. Sleep-based synchronisation is banned everywhere (§1.7). |
| Evidence | **Evidence** | Directory under `docs/test-results/` holding the artifacts. Convention in §1.6. |

The **Phase** column says when the test first runs (the phase that ships the code it needs); Phase 6
re-runs the whole catalogue under `-race` and re-verifies every crash boundary.

Rule for anyone running or extending this plan: **never weaken an assertion to make a test pass.**
A flaky lease test is a lease bug until proven otherwise; a suspected flaky test is reported as a
defect with a reproduction, not skipped, not `t.Skip`ed, not given a second chance with looser
expectations. The one sanctioned retry is caller-side retry of `database_busy` (`C-CLAIM-24`,
`C-ERR-7`) inside the code under test — and the tests assert that no other retry exists.

Upstream's 31 existing tests must pass **unmodified** for the whole life of this plan. Editing an
upstream test to keep it green is a compatibility-contract violation (`C-COMPAT-13`) and needs the
architect's sign-off recorded in the PR.

---

## 1. The harness

The harness is test infrastructure, not product: it exists so that every `MP` row in §2 can state a
mechanical, sleep-free construction instead of "spawn processes and hope". Five capabilities are
specified here; each is listed as a **Phase 2 build requirement** in §1.8 with the contract rules it
serves. Everything runs from `/tmp/ledgerboard-scratch/<test-id>/` — never inside `/home`
(disk: recon/baseline §2; ~100 MB free), and never committed (`.gitignore` covers `*.db*`).

### 1.1 The agent fleet — N independent agents as real processes

One server process under test + N **agent processes**, each a real OS process with its own HTTP
connection (and, where marked, its own MCP session or its own stdio server process — see below).

The agent process is `harness-agent`, a small test-only binary (built by the test into
`/tmp/ledgerboard-scratch/bin/`, via `go build -o … ./internal/harness/agent` — a `harnessbuild` tag,
never shipped). It reads a JSON-lines **script** on stdin and writes one JSON result line per op on
stdout. Ops:

| Op | Effect |
|---|---|
| `ready` | report configured base URL / stdio server, then wait for the controller |
| `claim {ticket, key}` · `claim_next {board, key}` · `renew {ticket, gen}` · `release {ticket, gen}` | one ownership request; the result line carries the full HTTP status, parsed `error_code`, `details` and body |
| `edit {ticket, fields, expected_version?, generation?}` | one claim-protected content write |
| `handoff_create {…, key}` · `handoff_accept {id, key}` · `note_add {ticket, body}` · `memory_submit {…, key}` | one handoff/memory/child write |
| `expect {status, code?, details_path?, value?}` | assert the *previous* result inline; mismatch ⇒ agent exits non-zero with the diff on its result line (the Go test then fails with the agent's own words) |
| `send_only` | **fire-and-report**: write the prepared request bytes to the socket, do **not** read the response, emit `sent` |
| `collect` | read the response of the earlier `send_only` and report it |
| `mark <name>` | write a line to the agent's marker FIFO (§1.5) for controller synchronisation |
| `wait_fifo <name>` | block until the controller (or another process) writes to the named FIFO |

**No-sleep synchronisation — the barrier.** The controller is the Go test. Sequence for a race:

1. controller starts N agents, each finishes `ready` (its result line arrives on the test's pipe) —
   readiness is a message, not a delay;
2. controller sends the request op to all N, followed by `send_only`;
3. controller blocks reading the N `sent` markers — **all N requests are now fully inside the server
   before any response is consumed**;
4. controller sends `collect` to all N, then reads N results.

This is the barrier/handshake the §21 claim race needs: the collision is *guaranteed* because every
request has already reached the server before the first one can complete and be observed. The
assertion is then about the **outcome contract** (exactly one winner, N−1 × 409 with the holder's
details), never about wall-clock order. If the implementation were a read-then-write (the upstream
defect, recon §4.4), multiple winners become possible and this construction fails the test — which is
the point of building it this way.

**Identity per agent.** Each agent sets its identity the way §C-AUTH-3 says callers do: REST agents
send `X-Ledgerboard-Actor-Type` / `X-Ledgerboard-Actor` per request; two of the N agents in the E2E
scenario run over **stdio MCP** instead — each spawns its own `kanban-mcp -mcp-transport stdio`
subprocess with `LEDGERBOARD_ACTOR_TYPE=agent`, `LEDGERBOARD_ACTOR=<id>`. A stdio agent's server is a
separate process with its own DB connection on the same SQLite file — the strongest legal reading of
"separate connections" (three server processes total in the E2E; multi-process single-writer safety
is itself contracted: `C-OUTBOX-17`, and the sweepers running per process are idempotent,
`C-CLAIM-19`).

### 1.2 The direct-DB harness — two processes, two connections, one file

For tests that must prove SQLite-level behaviour (busy handling, `RowsAffected()` racing, migration
watching) rather than transport behaviour: `harness-dbproc`, a test-only binary that opens its own
`*sql.DB` on the shared file **with the production DSN** (`C-CONV-9` — the test must never "help" by
using a different DSN; where a different DSN is the experiment, e.g. T-CLAIM-16's no-timeout
negative control, that is stated in the row). It speaks the same JSON-lines protocol, plus:

| Op | Effect |
|---|---|
| `sql {stmt, params}` | execute, report `rows_affected` / `last_insert_id` / error code |
| `tx_begin` · `tx_commit` · `tx_rollback` | hold a write transaction across ops — this is the busy-DB lock holder |
| `wait_column {table, column}` | poll `PRAGMA table_info` in a tight loop until the column exists (migration watcher, §2.7/T-CRASH-d); polling a local file-backed DB is not a sleep |
| `dump {tables}` | write a row-ordered (by `rowid`) JSON dump of the named tables for before/after comparison |

Two `harness-dbproc` processes barrier exactly like agents: both `ready` → both `send_only`-style
`armed` → controller `go` → both execute. SQLite's write lock serialises them; the outcome contract
(exactly one `RowsAffected()==1`) is checked, not timing.

### 1.3 The fake clock — how a lease expires without waiting 15 minutes

`now_ms` enters the code in exactly one place (`C-CONV-2`), so the clock is injectable in one place:

- **In-process (unit level):** the store takes a `Clock` interface (`NowMS() int64`) at construction;
  `store.New(db, clock)`. Production wiring uses `RealClock`. **Phase 2 build requirement** — it is
  the seam every expiry/fencing/backoff assertion needs, serving `C-CONV-1`, `C-CONV-2`, `C-CLAIM-14`,
  `C-OUTBOX-10`, `C-IDEM-16`.
- **In the server binary (integration/multi-process):** the **same production binary** is driven with
  `LEDGERBOARD_CLOCK=fake` and `LEDGERBOARD_CLOCK_FILE=<path>`. In fake mode every `NowMS()` call
  reads the 8-byte decimal file. Tests **advance time by writing the file** — no restart, no wait,
  and it works identically for every process watching that DB (the E2E's three server processes all
  read the same clock file). Default mode `LEDGERBOARD_CLOCK=real` keeps production behaviour
  byte-identical; the env is inert there.

Consequences the tests rely on:

- lease expiry = write `now_ms > lease_expires_at` into the clock file; the next request sees the
  lease lapsed (`C-CLAIM-1`) — the 15-minute wait is never simulated, it is *relabelled*;
- sub-second assertions are exact: INTEGER-ms lease columns are compared against the fake clock
  (`C-CONV-1` bans any TEXT↔INTEGER comparison; legacy TEXT columns are asserted only for
  *shape/equality*, never ordering);
- frozen time is a feature: with the clock frozen, two requests produce identical lease timestamps,
  which is how `C-CONV-2`'s "computed once per request" and `C-HANDOFF-20`'s equal-millisecond
  ordering tie are tested deterministically;
- the sweeper's 60-second ticker (`C-CLAIM-20`) is wall-time, not clock-time. Tests never wait for
  it: `LEDGERBOARD_SWEEP_INTERVAL_MS` (Phase 2 build requirement, default `60000`, startup-validated)
  is set to a small value in tests so the ticker fires promptly, while the sweep's *logic* is
  exercised deterministically by advancing the fake clock and/or invoking the store method directly
  (`C-CLAIM-19`: the sweep is optional for correctness and idempotent, so invoking it directly is
  testing the contract, not bypassing one).

### 1.4 Crash injection — kill at a named step, never at a random time

Every crash boundary in §21 names a step, and the harness kills exactly there. For code paths this
repo owns (claim, handoff, outbox, renew), the server carries an inert **crashpoint** facility:

- a `crashpoint` package with named points (`claim.pre_commit`, `claim.post_commit`,
  `handoff.after_handoff_insert`, `handoff.pre_commit`, `memory.post_adapter_write`,
  `memory.pre_ack_commit`, `renew.pre_commit`, `renew.post_commit`);
- each point is one call: `crashpoint.Hit("claim.pre_commit")` — a single atomic load of an armed-set
  parsed once at startup from `LEDGERBOARD_CRASH_POINTS="claim.pre_commit,…"`. Unarmed (default, and
  in production), the check is one atomic load and a branch — negligible and provably inert (the
  quality gate runs the full suite with no points armed);
- when armed, the hook **writes `crash <name> <seq>` to the crash FIFO, fsyncs it, then
  `syscall.Kill(os.Getpid(), SIGKILL)`**. SIGKILL, never `os.Exit` and never a panic: no deferred
  function may run, because defers committing or rolling back is precisely the behaviour under test.
- the Go test blocks reading the crash FIFO — synchronisation is a message, not a poll — then
  restarts the server on the same DB file and asserts the §3 matrix's recovery column.

**Phase 2 build requirement:** the crashpoint package plus the eight named points above. Each point
serves a rule: `claim.*` and `renew.*` → `C-CONV-3`/`C-AUDIT-4`/`C-IDEM-12` (one transaction per
mutation); `handoff.*` → `C-HANDOFF-14`/`C-IDEM-12`; `memory.*` → `C-OUTBOX-2`/`C-OUTBOX-9`.
Crash boundary (d) — *during migration* — cannot use in-process hooks, because the migration runner
is vendored `golang-migrate` code; the T-CRASH-d row in §3 specifies the watcher-landmark kill used
instead (named DDL-statement landmarks observed from a separate `harness-dbproc` watcher, SIGKILL
from the test at a chosen landmark).

### 1.5 Files, FIFOs and markers — the synchronisation alphabet

- **FIFOs** for events that must block a reader: crash markers (`crash.fifo`), agent readiness is
  carried on the agent's stdout pipe, outbox-delivery events on `outbox.fifo` (the **mock adapter**
  writes one line per `Write` call to it after fsyncing `calls.jsonl` — the test learns of a delivery
  the moment it happens).
- **Marker files** for state the test checks later (fsynced; existence is the assertion).
- **No sleeps anywhere in the harness.** The only waits are: blocking reads on pipes/FIFOs, bounded
  `busy_timeout` inside SQLite itself, and the two deliberately-slow cells of T-CLAIM-16 (5–6 s,
  documented, because the contract fixes `busy_timeout=5000` and the test must not lower it to make
  itself faster — that would be weakening the requirement).

### 1.6 Evidence convention

Every test writes to `docs/test-results/phase-<P>/<TEST-ID>/` (single-artifact tests may use a single
`.txt`):

- `command.txt` — the exact command line(s) and env, sufficient to re-run by hand;
- `output.txt` — the full run output with the exit code as the last line;
- extras per row: server log (stderr, truncated to the last 2000 lines), agent result lines,
  `harness-dbproc` dumps, SSE capture, the crash FIFO transcript, `PRAGMA foreign_key_check` output;
- `docs/test-results/phase-<P>/index.md` — the phase's index: one line per test ID with pass/fail;
  `summary.md` holds the §5 quality-gate outputs.

Evidence files are small text (logs capped); scratch DBs, binaries and dumps stay in
`/tmp/ledgerboard-scratch/` and are never committed. Nothing in `docs/test-results/` is hand-edited;
a human-meaningful note goes in the PR, not in the artifact.

### 1.7 Determinism rules (global)

1. No sleeps, no "wait for a bit". Blocking reads on pipes/FIFOs; SQLite's own `busy_timeout` is the
   only timed wait, and it is part of the contract under test (`C-CONV-9`, `C-ERR-7`).
2. All time is the fake clock (§1.3) wherever the assertion depends on time. Wall clock appears only
   in evidence timestamps.
3. Races assert **outcome contracts** (cardinality, error codes, post-state), never interleavings.
4. Retry loops exist only where a contract grants one (`database_busy` — `C-CLAIM-24`, `C-ERR-7`;
   outbox backoff — `C-OUTBOX-10`), are bounded, and their bound is asserted.
5. Sequences that exercise many states (T-CLAIM-18) use a **fixed-seed** PRNG — a written seed in the
   evidence, re-runnable bit-for-bit; "random" never means "not reproducible".
6. The whole catalogue passes under `CGO_ENABLED=1 go test -race ./...` (recon §10: `-race` needs
   cgo; the shipped binary is still `CGO_ENABLED=0` — §5 keeps the two apart).

### 1.8 Phase 2 must build first (harness capabilities)

| # | Capability | Phase | Serves |
|---|---|---|---|
| H1 | `Clock` interface + `store.New(db, clock)` injection; `RealClock` default | 2 | `C-CONV-1`, `C-CONV-2` |
| H2 | `LEDGERBOARD_CLOCK=fake` + `LEDGERBOARD_CLOCK_FILE` in the server binary | 2 | `C-CONV-2`, `C-CLAIM-14`, `C-FENCE-10`, `C-IDEM-16`, `C-OUTBOX-10` |
| H3 | `crashpoint` package + the eight named points (§1.4) | 2 | `C-CONV-3`, `C-AUDIT-4`, `C-IDEM-12`, `C-HANDOFF-14`, `C-OUTBOX-2`, `C-OUTBOX-9` |
| H4 | `harness-agent` + `harness-dbproc` binaries and the JSON-lines protocol (§1.1/§1.2) | 2 | `C-CLAIM-5`, `C-INV-1`, `C-CONV-9`, `C-ERR-7` |
| H5 | `LEDGERBOARD_SWEEP_INTERVAL_MS` config knob, default `60000`, validated at startup | 2 | `C-CLAIM-20` (its default is the contract; the knob exists so tests can bound the ticker) |
| H6 | Ephemeral listening port for tests (`--port 0` writes the chosen port to stdout/log as JSON) | 2 | transports generally; `C-COMPAT-11` unaffected (default bind stays loopback) |
| H7 | `LEDGERBOARD_LEASE_TTL_MS` config knob, default `900000`, validated (min 1000) | 2 | named here because contracts.md names no TTL knob (§9 gap G4); default is the spec's 15 min |
| H8 | Mock memory adapter behind `LEDGERBOARD_MEMORY_ADAPTER=mock` (§1.9) | 5 (interface seam in 2) | `C-OUTBOX-19`–`C-OUTBOX-22`, `C-IDEM-5` |

### 1.9 The mock memory adapter — swapped in by configuration

`LEDGERBOARD_MEMORY_ADAPTER` accepts exactly `off` (no adapter; legal only with
`MEMORY_DELIVERY=off`), `mock` (test), or the real adapter once it exists (`C-OUTBOX-21`).
`LEDGERBOARD_MOCK_ADAPTER_DIR=<dir>` configures the mock, which implements the full `Adapter`
interface of `C-OUTBOX-19` **plus** `Searcher`:

- `calls.jsonl` — one fsynced JSON line per `Write`: `{id, type, content, confidence, tags, ts,
  external_id}`. The test parses it to assert what the far end received.
- `behavior.json` — read **on every call** (tests rewrite it live, no restarts):
  `{mode: ok|retryable|permanent|timeout|hang, fail_first: N, external_id_prefix}`.
  `retryable`/`permanent`/`timeout` return the corresponding `*DeliveryError` codes from `C-OUTBOX-11`
  — the mock proves the worker classifies by **error type, never by string matching**.
- **Dedup by stable id:** the same `id` twice → the same `external_id`, one record. This is the far
  end of `C-IDEM-5`/`C-OUTBOX-8` (at-least-once is safe because the id is the idempotency key), and
  the E2E's memory-outage leg asserts the *board side* of it: the duplicate delivery carries the
  identical id.
- `corpus.json` — optional records so `Search` returns real results; absent corpus ⇒ the mock still
  implements `Searcher` but returns zero records (distinct from `search_unsupported`, `C-OUTBOX-20`).

The mock never sees the DB or the ticket row (`C-OUTBOX-22`); it is wired only through `MemoryWrite`.
