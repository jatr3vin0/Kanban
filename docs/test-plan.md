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

---

## 2. Test catalogue

### 2.1 Regression — upstream surface stays working

The §21 regression line is: *upstream suite + board/epic/ticket/note/relation/ready/context/MCP/REST/SSE*.

| ID | Rules | Asserts | Level · why | Det. | Evidence | Ph |
|---|---|---|---|---|---|---|
| T-REG-01 upstream suite green | `C-COMPAT-13` | `go test ./... -count=1` passes with all 31 upstream test functions named in recon §6 passing **unmodified**; any edit to an upstream test file fails the phase | `I` — it is the vendor's own suite; run as-is | none needed (deterministic suite) | `phase-<P>/T-REG-01/` (go test -v log) | 2 |
| T-REG-02 board+epic CRUD | `C-COMPAT-1`, `C-OPEN-7`, `C-AUTH-9` | `POST /api/v1/boards` → 200 with bare object (`id`,`name`,`description`,`created_at`,`updated_at` at top level, no `ok`/`data`); agent actor on `POST /boards` → 403; human/orchestrator → 200; same for epics | `I` — pure request/response shape | fixed payloads | `T-REG-02/` | 3 |
| T-REG-03 ticket+children CRUD | `C-COMPAT-1`, `C-COMPAT-2`, `C-GUARD-11`, `C-GUARD-12` | ticket create/update/delete, task/note/relation create/update/delete all 2xx with bare shapes; `{"status":"in_progress"}` → 200 and `{"status":"in progress"}` → 400 with the unchanged message; duplicate relation → 409; editing `assignee` never touches `lease_holder` (row compared before/after) | `I` | fixed payloads | `T-REG-03/` | 3 |
| T-REG-04 ready/context/summary | `C-COMPAT-1`, `C-CLAIM-12` (priority CASE shared) | `GET /boards/{id}/ready` excludes blocked/dependent and leases-lapsed tickets only per `C-CLAIM-1`; context and summary return bare objects; ready order = priority weight `critical<high<medium<low`, then `created_at`, then `id` | `I` | seeded fixture, fake clock frozen | `T-REG-04/` | 3 |
| T-REG-05 MCP six tools | `C-COMPAT-9`, `C-ERR-13` | `tools/list` contains the six original tools with unchanged names/required args; scripted call to **every original action** succeeds over both HTTP and stdio; success shapes unchanged (bare object / `{"items":[…]}`); annotations may change only per `C-COMPAT-9` | `I` — annotations are wire facts | fixed script per action | `T-REG-05/` (tools/list JSON + transcript) | 3 |
| T-REG-06 REST/MCP parity of ownership ops | `C-COMPAT-8`, `C-ERR-10`–`C-ERR-13`, `C-CONV-7` | the same claim/renew/release/handoff/memory call succeeds over REST **and** MCP with the same `error_code` on the failure twin; a mutation made over MCP emits exactly one `board_change` for its board (upstream bug fixed) | `I` — parity is the rule | same fixture run twice | `T-REG-06/` | 3 |
| T-REG-07 SSE shape + publish-after-commit | `C-COMPAT-7`, `C-COMPAT-8`, `C-CLAIM-10`, `C-IDEM-9` | subscribe, mutate (REST and MCP), assert `event: board_change` and `board_id` present and correct; an idempotent replay emits **no** second event; a rolled-back mutation emits none | `I` — SSE is a transport behaviour | SSE read via blocking channel with a bounded `select` deadline (the one sanctioned deadline; failure = no event within 2 s) | `T-REG-07/` (SSE capture) | 3 |
| T-REG-08 error body shape | `C-COMPAT-10`, `C-ERR-1`–`C-ERR-15` | for every reachable code in the §7.3 table: correct REST status, `error` string present, `error_code` present, `details` per that code's "required details" column, `request_id` always present; `Content-Type: application/json`; no response anywhere in the phase's captured traffic emits a reserved code (`unauthorized`, `rate_limited`, `payload_too_large`, `idempotency_in_progress`) | `I` | each code provoked by the exact input named in contracts §7.3 | `T-ERR-01/` shares artifacts | 3 |
| T-REG-09 timestamps stop changing shape | `C-COMPAT-6`, `C-CONV-8`, `C-CONV-1` | create → response `created_at` == `GET` → `created_at`, byte-equal; new ms fields render `2026-…​.180Z` (3 fractional digits); no TEXT↔INTEGER time comparison exists (grep gate in §5) | `I` | fixed | `T-REG-09/` | 3 |
| T-REG-10 default bind loopback | `C-COMPAT-11`, `C-AUTH-4` | start with no flags → listener on `127.0.0.1` only; `--host 0.0.0.0` → all interfaces; non-loopback bind without `--trust-actor-header` → actor headers ignored (identity = `agent:local` default) | `I` | probe `net.Listen` state via log line | `T-REG-10/` (also `phase-0/bind-address-probe.txt` precedent) | 3 |
| T-REG-11 legacy rows readable | `C-COMPAT-12`, `C-CONV-5` | ticket with `status='weird'` inserted directly: `GET` → 200 with raw status; `PUT {"status":"weird"}` → 400 | `U`+`I` — direct-SQL fixture is the only way to create it | fixed | `T-REG-11/` | 3 |
| T-REG-12 unknown-field warn/strict | `C-COMPAT-3`, `C-ERR-9`, `C-FENCE-12`, `C-HANDOFF-4` | `PUT /tickets/{id}` with an unknown field → 200 + `warnings:[…]` (default) and 400 `unknown_field` under strict mode; body fields `actor`/`generation`/`lease_*` are always ignored **and warned** | `I` | fixed | `T-REG-12/` | 3 |
| T-REG-13 MCP stdio identity | `C-AUTH-3`, `C-AUTH-6` | stdio server started with `LEDGERBOARD_ACTOR_TYPE=agent`, `LEDGERBOARD_ACTOR=builder-9`: created ticket's audit row `actor = agent:builder-9`; a client sending `actor_type=system` anywhere → 403 | `I` | fixed | `T-REG-13/` | 3 |
| T-REG-14 route/tool inventory diff | `C-OPEN-9`, `C-OPEN-10`, `C-OPEN-12` | scripted diff of the server's registered REST routes + `tools/list` actions against `concurrency.md` §2–§6: every route/action appears exactly once, no extra, no missing; every *not audited* cell is a read or a `C-AUDIT-9` gap | `I` | mechanical diff | `T-REG-14/` (diff output) | 3 |

### 2.2 Claims — the ownership engine

| ID | Rules | Asserts | Level · why | Det. | Evidence | Ph |
|---|---|---|---|---|---|---|
| T-CLAIM-01 five concurrent claimants, one ticket | `C-CLAIM-5`, `C-CLAIM-9`, `C-INV-1`, `C-ERR-4`, `C-ERR-14`, `C-CONV-3` | 6 agents (`harness-agent` processes, separate connections) race `POST /tickets/{t}/claim` on one ticket via the `send_only`/`collect` barrier: **exactly one** 200 whose body carries the winner's `lease_holder`, `lease_expires_at = now+TTL`, `lease_generation = g0+1`, `version = v0+1`; the other **five** are `409 already_claimed` whose `details` carry the full `C-ERR-4` set with the *winner's* `holder`/`lease_generation`/`lease_expires_at`; after the dust: `lease_holder IS NOT NULL`, `lease_generation = g0+1` exactly | `MP` — the whole rule is about separate processes losing the race; goroutines on one handle cannot prove `C-INV-1` | barrier §1.1(1–4); outcome asserted, not order | `phase-2/T-CLAIM-01/` | 2 |
| T-CLAIM-02 multi-ticket claim_next race | `C-CLAIM-12`, `C-CLAIM-13`, `C-INV-1`, `C-CLAIM-7` | board holds 5 claimable tickets (2 critical, 2 medium, 1 low; distinct `created_at`); 5 agents race `claim_next`: 5 × 200, all **distinct** tickets, the two critical ones claimed first under the `priority CASE, created_at, id` order (winner set = the 5 tickets, order verified from audit `created` order via `rowid`, `C-AUDIT-7`); 6th agent → `404 not_found` `details.reason="no_claimable_ticket"`; no ticket holds two holders | `MP` | barrier; fake clock frozen so `created_at` ties are exercised through the `id` tiebreak | `T-CLAIM-02/` | 2 |
| T-CLAIM-03 claim success shape | `C-CLAIM-6`, `C-CLAIM-9`, `C-CLAIM-10`, `C-CONV-7`, `C-AUDIT-2`, `C-AUDIT-4`, `C-COMPAT-8`, `C-AUDIT-8` | claim on unclaimed ticket → 200; response is the whole entity incl. `version` and `claim` block; `status='in_progress'`; `ticket_events` has one `claimed` row written in the same transaction with `request_id` and post-`generation` in the payload, `lease_expires_at` as ms integer; one SSE event after commit; `updated_at` stays TEXT seconds (`C-CONV-1`) | `U`+`I` — shape is unit-provable; SSE timing is integration | fake clock frozen → both audit `created_at` and payload ms deterministic | `T-CLAIM-03/` | 2 |
| T-CLAIM-04 claim refusal diagnosis | `C-CLAIM-2`, `C-CLAIM-8`, `C-CLAIM-22`, `C-ERR-15`, `C-CLAIM-18` | on `done` → `409 status_not_claimable`; on `blocked` → `409 status_not_claimable`; with unfinished blocker (and blocker itself `blocked`) → `409 dependency_blocked` carrying `blocking_ticket_ids[]`; absent id → `404 not_found` **before** any 409 diagnosis; unknown id never yields a 409 | `U` — diagnosis order is a single decision table; direct-SQL fixtures set each precondition exactly | fixed fixtures | `T-CLAIM-04/` | 2 |
| T-CLAIM-05 claimability config + lapsed three-way | `C-CLAIM-3`, `C-CLAIM-1`, `C-CLAIM-7`, `C-CONV-1` | default config claims `todo`; `LEDGERBOARD_CLAIMABLE_STATUSES` accepts a custom list and **rejects** `done`/`blocked` at startup; a ticket left `in_progress` by a lapsed lease (no sweeper ever ran) is claimable — claim with clock advanced past `lease_expires_at` → 200, new generation = old+1; a third arm: human-moved `in_progress` with no holder → claimable | `I` — config is a process-level fact; fake clock is binary-level (§1.3) | clock file advance; no sweeper involved (interval knob untouched) | `T-CLAIM-05/` | 2 |
| T-CLAIM-06 capability fences | `C-CLAIM-11`, `C-AUTH-4`, `C-AUTH-6`, `C-AUTH-7`, `C-OPEN-7` | `human` claim → `403 forbidden`; `agent` reassign/revoke/unblock/board-create/epic-create/outbox-retry → `403` each; client presenting `system` → 403; every one of these is 403 **never** 409/404 | `I` | identity via headers per `C-AUTH-3` | `T-CLAIM-06/` | 3 |
| T-CLAIM-07 renew heartbeat | `C-CLAIM-15`, `C-INV-6`, `C-FENCE-4`, `C-AUDIT-2` | holder renews → 200; `lease_expires_at = now+TTL` recomputed, `lease_renewals +1`, `version` unchanged, `lease_generation` unchanged; `claim_renewed` audit row with `previous_expires_at`/`renewals` | `U`+`I` | fake clock: renew at fixed offsets; expiry deltas exact in ms | `T-CLAIM-07/` | 2 |
| T-CLAIM-08 renew never resurrects; renew-vs-sweep | `C-CLAIM-14`, `C-FENCE-10`, `C-CLAIM-21` | (i) claim, advance clock past expiry, renew → `409 lease_expired` with `details.lease_expires_at`, and the row is untouched (still lapsed holder, same generation — a sweep hasn't run); (ii) 6 tickets at expiry boundary: concurrent renew×holder vs sweep across processes — every outcome is 200-renew or 409-lease_expired or swept-clear, **never** 500, and post-state satisfies `C-INV-3`/`C-INV-4` for each ticket | (i) `U`; (ii) `MP` — the race is SQLite-lock-level | (i) clock advance; (ii) barrier + sweep invoked via store in a second process at the same frozen boundary | `T-CLAIM-08/` | 2 |
| T-CLAIM-09 renew guard order | `C-CLAIM-16`, `C-FENCE-2`, `C-CLAIM-22` | agent B reusing agent A's *generation number* on the same ticket → `409 not_claim_holder` (holder mismatch is diagnosed first); A with stale generation → `409 lease_generation_mismatch` carrying `expected_generation`/`current_generation`; absent ticket → 404 first | `U` | fixed | `T-CLAIM-09/` | 2 |
| T-CLAIM-10 release + stale-token death | `C-CLAIM-6`, `C-CLAIM-17`, `C-FENCE-9`, `C-INV-2`, `C-INV-3`, `C-INV-6` | holder releases → holder/type/acquired/expires all NULL, renewals 0, `status in_progress→todo`, generation +1, version +1, all in one row (`C-INV-3`); then the *old* holder's claim-protected edit with its old generation → rejected (ticket unclaimed ⇒ guard's holder arm fails with old generation; if a new holder exists ⇒ `already_claimed`; if NULL-holder-with-stale-gen is presented it is **not** authorised: holder arm requires `lease_holder = :actor` which NULL ≠ actor) — assert 409 family per diagnosis table; release by non-holder → `409 not_claim_holder`; release with stale gen → `409 lease_generation_mismatch` | `U`+`MP` (MP variant: release racing a stale write in two processes, outcome only ever release-wins-then-reject) | barrier for the MP twin | `T-CLAIM-10/` | 2 |
| T-CLAIM-11 lapsed-holder release — **flagged ambiguity** | `C-CLAIM-18` (ambiguous — see §9 G1), transition table T3 | the transition table T3 allows `release_claim` from `LAPSED`; the §2.5 release SQL has no expiry guard ⇒ a lapsed holder's release affects 1 row (200, cleared, generation bumped). Test asserts **the SQL-normative reading**: lapsed holder release → 200 + cleared. If the architect resolves `C-CLAIM-18` the other way, this row flips to assert `409 lease_expired` — one assertion change, recorded in the evidence header | `U` | clock advance | `T-CLAIM-11/` | 2 |
| T-CLAIM-12 revoke | `C-CLAIM-17` (T6), `C-AUTH-7`, `C-ERR-15` | human revoke on claimed ticket → 200, holder cleared, generation +1, version +1, `claim_revoked` audit; revoke on unclaimed → `409 not_claimed`; agent revoke → 403 | `I` | fixed | `T-CLAIM-12/` | 3 |
| T-CLAIM-13 reassign + override gate | `C-CLAIM-5` (T5), `C-GUARD-7`, `C-GUARD-8`, `C-AUTH-10`, `C-ERR-4` | human `reassign` over live lease without `override:true` → `409 already_claimed` with `details.override_required=true`; with `override:true` → 200, new holder, generation +1; reassign to self (T8) → 200, holder unchanged but generation +1; `claim_reassigned` audit with `override:true` and `from_holder` | `I` | fixed | `T-CLAIM-13/` | 3 |
| T-CLAIM-14 fencing after override | `C-FENCE-9`, `C-FENCE-2`, `C-FENCE-3`, `C-GUARD-3`, `C-ERR-4` | A holds ticket (gen g, version v). Human reassigns **to A itself** (override) → generation g+1. A's next edit with `generation:g` → `409 lease_generation_mismatch` (expected g, current g+1); A's edit with correct gen but stale `expected_version` → `409 version_conflict`; **B's** edit on A's live ticket → `409 already_claimed` with A's details. This is the reachable stale-write path (see §9 G6 note) | `I` — the three diagnosis branches of one guard | fixed | `T-CLAIM-14/` | 3 |
| T-CLAIM-15 expiry sweep | `C-CLAIM-19`, `C-CLAIM-20`, `C-CLAIM-6`, `C-INV-2`, `C-AUDIT-1`, `C-ERR-14` | sweep (store call + `system` actor) returns ids of exactly the lapsed tickets via `RETURNING`; each: holder cleared, `in_progress→todo`, generation +1, version +1; `done`/`blocked`/`todo` untouched; running twice affects 0 rows the second time; audit rows carry `reason:"expiry"` and `actor=system:sweeper`-form id | `U` — single-statement semantics; MP coverage of sweep-vs-renew lives in T-CLAIM-08(ii) | fake clock sets exact lapse instants | `T-CLAIM-15/` | 2 |
| T-CLAIM-16 busy DB | `C-CONV-9`, `C-ERR-7`, `C-CLAIM-24`, `C-INV-5` | (i) `harness-dbproc` holds `BEGIN IMMEDIATE` on the file; agent claims; controller tells holder to commit; claim **succeeds** after waiting (busy window < `busy_timeout=5000`); (ii) holder holds 6 s (> `busy_timeout`): the claim returns **`503 database_busy`** with `Retry-After: 1` — never a bare 500, never a silent success; (iii) negative control (evidence only, run once): 4 `harness-dbproc` processes × 10 UPDATEs each with the contract DSN → 40/40 ok (recon §4.5 measured 17/40 lost upstream; the harness must be able to detect the disease it exists to prove cured) | `MP` — busy is a cross-connection phenomenon | lock holder releases on FIFO command (no sleeps in the test; the 6 s hold in (ii) is the *workload*, and is the one deliberately-slow cell, documented) | `T-CLAIM-16/` | 2 |
| T-CLAIM-17 generation monotonicity | `C-INV-2`, `C-FENCE-8`, `C-FENCE-10`, `C-CLAIM-4` | scripted chain claim→release→claim→reassign(self)→renew→sweep→claim: after each step `lease_generation` = previous+1 (renew: unchanged), never anything else; direct-SQL check that no statement writes a literal generation (grep gate §5: `lease_generation = lease_generation + 1` only) | `U` | fake clock | `T-CLAIM-17/` | 2 |
| T-CLAIM-18 invariant property sweep | `C-INV-1`–`C-INV-8`, `C-CLAIM-4`, `C-CLAIM-6` | run ~200 seeded op sequences (fixed seed recorded) over a 5-ticket board mixing claim/renew/release/reassign/revoke/edit/status moves/sweep; after **every** op, assert in SQL: at most one holder (`C-INV-1`); generation monotonic +1 or unchanged (`C-INV-2`); `lease_holder IS NULL ⇒` all five lease fields per `C-INV-3`; live lease ⇒ `status='in_progress'` (`C-INV-4`); version delta ∈ {0 (renew), 1 (accepted mutation)} (`C-INV-6`); any violation prints the full op log | `U` — invariants are row-local; the sequences exist for coverage, not for timing | fixed seed; fake clock | `T-CLAIM-18/` (op log + final dump) | 2 |
| T-CLAIM-19 version semantics | `C-FENCE-3`, `C-FENCE-5`, `C-FENCE-6`, `C-FENCE-7`, `C-GUARD-4`, `C-INV-6`, `C-OPEN-6` | agent edit without `expected_version` → `400 invalid_request` (`details.field`); agent edit with stale version → `409 version_conflict` (`expected_version`,`current_version` in details); accepted edit bumps version exactly 1 regardless of field count; renew bumps 0; human edit **without** `expected_version` → 200 last-write-wins audited with `previous_version`; human with stale version → 409 like anyone | `U`+`I` | fixed | `T-CLAIM-19/` | 2 |
| T-CLAIM-20 protected-vs-open matrix | `C-GUARD-1`, `C-GUARD-2`, `C-GUARD-3`, `C-OPEN-1`, `C-OPEN-2`, `C-OPEN-3`, `C-OPEN-4`, `C-OPEN-5`, `C-GUARD-12` | on a claimed ticket, non-holder agent: note-add 200, task-add 200, task-tick 200, relation-add 200 (open); content edit → 409; ticket delete → 409; on an **unclaimed** ticket: agent content edit with valid `expected_version` → 200 (open); task/note/relation writes carry no `expected_version` requirement (send none → 200); `assignee` edit by anyone never grants ownership (no `lease_*` change) | `I` — the matrix is behavioural | fixed | `T-CLAIM-20/` | 3 |
| T-CLAIM-21 claim idempotency | `C-IDEM-1`, `C-IDEM-4`, `C-IDEM-6`, `C-IDEM-7`, `C-IDEM-8`, `C-IDEM-9`, `C-IDEM-15`, `C-SCHEMA-17`, `C-IDEM-17` | claim without key → `400 invalid_request` `details.field="idempotency_key"`; same key+same payload after success → byte-identical replayed body + `Idempotency-Replayed: true`, **no** second audit row, no SSE, generation **not** bumped again; same key+different payload → `409 idempotency_conflict` with the *stored* `request_hash`; a 4xx claim stores nothing (retry re-executes); two concurrent same-key claims (two processes, busy-lock delay on the first) → second **replays** the first's response, and `503 database_busy` appears only if the wait exceeded `busy_timeout`; the key never appears in any audit payload | `U`+`MP` (the concurrent-replay leg is `MP`) | barrier; busy-holder protocol from T-CLAIM-16(i) makes the "blocks then replays" ordering exact | `T-CLAIM-21/` | 2 |
| T-CLAIM-22 stale holder after re-claim | `C-FENCE-2`, `C-FENCE-9`, `C-ERR-4`, `C-GUARD-3` | A claims (gen g), clock advances past TTL, B claims (gen g+2 after sweep/re-claim path), A's edit with `generation:g` → `409 already_claimed` with **B's** holder details (diagnosis: not-the-holder precedes generation mismatch, `C-CLAIM-22`); the response never contains A's generation as current | `U` | clock advance | `T-CLAIM-22/` | 2 |

### 2.3 Idempotency — the cross-cutting contract

The operation-specific legs live in T-CLAIM-21 (claim), T-HAND-10 (handoff), T-MEM (memory). These
rows prove the storage/semantics rules themselves.

| ID | Rules | Asserts | Level · why | Det. | Evidence | Ph |
|---|---|---|---|---|---|---|
| T-IDEM-01 scope + key validation | `C-IDEM-1`, `C-IDEM-2`, `C-IDEM-3`, `C-IDEM-4` | same `key` string under two different actors → both execute independently; one actor, same key, two operations (claim vs handoff.create) → independent; key charset: 128-char key OK, 129 → 400, key with `sp ace`/`/`/`?` → 400 `details.field="idempotency_key"`; `memory.write.deliver` refuses client keys (internal op not on any transport — asserted by route inventory T-REG-14) | `U`+`I` | fixed | `T-IDEM-01/` | 2 |
| T-IDEM-02 replay is byte-identical and inert | `C-IDEM-6`, `C-IDEM-9`, `C-IDEM-10`, `C-SCHEMA-18`, `C-IDEM-17` | create ticket with key K, then edit the ticket, then re-send create with K → response is the **original** body bytes (stored `response_body` verbatim), same status, `replayed:true`; no new audit row, no SSE, no second ticket; reordering the same JSON fields (same canonical form) still replays (hash is canonical, not raw bytes — `C-IDEM-10`); the key never appears in audit payloads or error details other than `C-IDEM-7`'s | `U`+`I` | frozen clock makes bodies reproducible byte-for-byte | `T-IDEM-02/` | 2 |
| T-IDEM-03 2xx-only recording | `C-IDEM-8`, `C-IDEM-13` | claim with key K that fails `409 already_claimed` → no key row stored (direct DB check); the retried claim that then succeeds stores one; a `500` path (forced by busy-holder timeout in a scratch env) stores nothing | `U` | busy-holder from T-CLAIM-16 to force the 5xx deterministically | `T-IDEM-03/` | 2 |
| T-IDEM-04 retention + purge | `C-IDEM-16`, `C-IDEM-18` | insert key rows, age the fake clock past 72 h, trigger purge (startup path + in-transaction opportunistic path): rows with `expires_at < now` deleted; the entities they created are untouched; after purge the same key executes as a new request; a purge never runs inside the request being deduplicated | `U` | fake clock; purge invoked via its startup/in-tx entry points | `T-IDEM-04/` | 2 |
| T-IDEM-05 crash atomicity of key+mutation | `C-IDEM-12`, `C-IDEM-13`, `C-IDEM-14` | with the crashpoint armed: T-CRASH-a's pre-commit kill leaves **neither** key row nor mutation; post-commit kill leaves both; no third state ever observed across 20 kill iterations; `status='in_flight'` is never observable after restart | `MP` | crash FIFO (§1.4); iteration count is the determinism argument | shares `T-CRASH-a/` | 2 |

### 2.4 Handoffs

| ID | Rules | Asserts | Level · why | Det. | Evidence | Ph |
|---|---|---|---|---|---|---|
| T-HAND-01 validation matrix | `C-HANDOFF-1`, `C-HANDOFF-2`, `C-HANDOFF-3`, `C-HANDOFF-4`, `C-HANDOFF-5`, `C-HANDOFF-6`, `C-HANDOFF-7`, `C-SCHEMA-12`, §5.2 field table | each rule one sub-assert, each returning `400 handoff_invalid` with the exact `details.field`: `schema_version=2`; empty/whitespace `summary`; 4001-char summary; canonical JSON > 65536 bytes; `memory_writes[3].type="factoid"` with `details.allowed=[…]`; confidence −0.1 / 1.1 / `"0.9"` / NaN-ish; 17 tags; 65-char tag; tag with bad charset; `to_actor` 201 chars; `supersedes` = own id; artifact `ref` with `user:pass@` URL; `memory_writes[].content` 2001 chars; done[] 51 items; unknown field warn/strict per `C-HANDOFF-4`; **the identical matrix over MCP returns the same `error_code`** (`C-HANDOFF-1`: validation at the service boundary) | `U` (matrix) + `I` (MCP parity) | table-driven, fixed inputs | `T-HAND-01/` | 2 |
| T-HAND-02 creation is claim-protected | `C-HANDOFF-13`, `C-FENCE-2`, `C-FENCE-5`, `C-AUTH-7` | agent non-holder → `409 already_claimed`; agent with holder+generation → 200; agent **without** `generation` field → `400 invalid_request` (missing, not conflict); human without any lease → 200 and the audit row says `from_actor_type=human`; orchestrator holding the lease → 200 fenced | `I` | fixed | `T-HAND-02/` | 2 |
| T-HAND-03 author keeps the lease | `C-HANDOFF-15`, `C-INV-2`, `C-INV-6` | after a successful handoff create: holder unchanged, generation unchanged, version unchanged (or +0); a second handoff by the same author succeeds immediately; both appear in history newest-first | `U`+`I` | fixed | `T-HAND-03/` | 2 |
| T-HAND-04 immutability + checksum | `C-HANDOFF-10`, `C-SCHEMA-9`, `C-SCHEMA-10`, `C-HANDOFF-9` | no update/delete route exists (T-REG-14 diff proves absence); after `accept`, re-`GET` the document: every author-controlled field byte-identical to the creation response except `status`/`accepted_by`/`accepted_at`; `checksum` recomputed from the stored typed columns equals the stored value (field-set per §9 G2 resolution — Phase 2 must decide and document whether `status`/`accepted_*` are inside the checksummed canonical form; the test asserts **whichever is decided**, and the decision is written into the evidence header) | `U` | frozen clock | `T-HAND-04/` | 2 |
| T-HAND-05 outbox atomicity at create | `C-HANDOFF-14`, `C-SCHEMA-13`, `C-SCHEMA-14`, `C-OUTBOX-15`, `C-AUDIT-2` | handoff with K=3 memory_writes → one transaction writes: handoff row, 3 outbox rows (ids = stable write ids), `outbox_enqueued` audit with `count` and `write_ids[]`; a handoff whose 3rd memory_write is invalid (bad type) writes **nothing** — no handoff, no partial outbox rows (validated before insert; the crash-twin is T-CRASH-b); outbox rows carry **no FK** to tickets — deleting the ticket leaves the outbox rows (`C-SCHEMA-13`) | `U` + `I` for the delete leg | fixed | `T-HAND-05/` | 2 |
| T-HAND-06 accept = ownership transfer | `C-HANDOFF-16`, `C-HANDOFF-17`, `C-HANDOFF-18`, `C-SCHEMA-11`, `C-AUDIT-2` | offered-to-specific acceptor: that actor accepts **without holding the lease** → 200; row now: holder=acceptor, type, acquired/expires = now/now+TTL, generation +1, `status='in_progress'`, `version +1`, `tickets.handoff_id` = id; handoff row: `accepted`, `accepted_by`, `accepted_at` ms; accept by the wrong actor → `409 handoff_not_acceptable`; second accept → 409 (no longer offered); accept offered-to-null by whoever claims next → 200; superseding does not move `tickets.handoff_id` (`C-HANDOFF-18`) | `I` | frozen clock → `accepted_at` exact | `T-HAND-06/` | 2 |
| T-HAND-07 supersession chains | `C-HANDOFF-11`, `C-HANDOFF-12`, `C-SCHEMA-12` | correction A2 `supersedes` A1 → A1 `status='superseded'` in the same transaction, A2 `offered`; 3-chain walks newest-first; self-reference → 400; cross-ticket supersedes → 400; a would-be cycle (A3 supersedes A1 which is already superseded by A2… chain re-target) → 400 with no partial state | `U` | fixed | `T-HAND-07/` | 2 |
| T-HAND-08 retrieval shapes + pagination | `C-HANDOFF-19`, `C-HANDOFF-20`, `C-HANDOFF-21`, §5.6 | latest/history/by-id/compact on REST **and** MCP with identical field names; history default limit 20, max 100 (>100 → clamped or 400 per decided behaviour — asserted as decided); `before` cursor + `next_before` walks the full history in order; ordering `created_at DESC, id DESC` proven with two handoffs at the **same fake millisecond** (id tiebreak); compact shape: counts not lists, `summary` truncated at 500 with `summary_truncated:true`; no-handoff ticket → latest 404, history `[]` | `I` | frozen clock creates the exact tie | `T-HAND-08/` | 2 |
| T-HAND-09 restart survival | `C-HANDOFF-14`, `C-OUTBOX-15`, `C-COMPAT-4` | create handoff (3 writes), SIGKILL the server **post-commit** (crashpoint `claim.post_commit`-style generic point `tx.post_commit`), restart on same DB: handoff + 3 outbox rows + audit intact byte-identical; with `MEMORY_DELIVERY=worker` the backlog delivers in `created_at` order after restart (`C-OUTBOX-16`) | `MP` | post-commit crashpoint; FIFO sync | `T-HAND-09/` | 2 (delivery leg 5) |
| T-HAND-10 handoff idempotency + accept race | `C-IDEM-6`, `C-IDEM-9`, `C-HANDOFF-16`, `C-CLAIM-5`, `C-INV-1` | create with key replayed → byte-identical, no second handoff; accept with same key replayed → no second generation bump, no second `handoff_accepted` audit; **two agents race accept of one offered-to-null handoff** (MP barrier): exactly one 200 + holder, the other `409 handoff_not_acceptable`; generation incremented exactly once | `U` + `MP` | barrier §1.1 | `T-HAND-10/` | 2 |

### 2.5 Memory / outbox

| ID | Rules | Asserts | Level · why | Det. | Evidence | Ph |
|---|---|---|---|---|---|---|
| T-MEM-01 wiring and modes | `C-OUTBOX-14`, `C-OUTBOX-15`, `C-OUTBOX-17`, `C-OUTBOX-18`, `C-OUTBOX-21` | `MEMORY_DELIVERY` default `off`; value `banana` → startup error; `worker` with no adapter → startup error; `off`: no worker goroutine (no delivery attempts in evidence window) and rows still created and listed by `GET /api/v1/outbox` (backlog visible); `worker`: one loop claims one row at a time; a second process is safe (both run; rows never double-processed) | `I` — startup behaviour is process-level | mock dir behaviors; FIFO for delivery events | `T-MEM-01/` | 5 |
| T-MEM-02 success path | `C-OUTBOX-2`, `C-OUTBOX-24`, `C-SCHEMA-14`, `C-IDEM-5`, §6.2 fields | `worker` + mock `ok`: row pending → processing (attempts incremented **at claim**) → succeeded; `external_id` = mock's deduped id; `delivered_at` ms set; audit `memory_delivered` with `write_id`/`external_id`/`attempts`; `mock calls.jsonl` shows `MemoryWrite` fields exactly (`id` == outbox id) and nothing more (`C-OUTBOX-22`) | `I` | mock FIFO signals the test at the delivery instant | `T-MEM-02/` | 5 |
| T-MEM-03 failure classification | `C-OUTBOX-10`, `C-OUTBOX-11`, `C-OUTBOX-24`, `C-SCHEMA-16` | mock `retryable` → status `retryable`, `next_attempt_at = now + 30_000` (attempt 1), `last_error` ≤ 500 chars and **scrubbed** (feed the mock an error string containing `Authorization: Bearer x` — stored `last_error` contains no credential substring); mock `permanent` (`Retryable=false`) → `failed` immediately + audit `memory_delivery_failed`; mock unclassified error / timeout-after-send → `uncertain` (`C-OUTBOX-11` third clause); classification never inspects strings (evidence: two different messages with the same `DeliveryError.Code` behave identically) | `I` | behavior.json rewritten between legs; fake clock advances backoff | `T-MEM-03/` | 5 |
| T-MEM-04 retry schedule + exhaustion | `C-OUTBOX-10` | fake clock advances through the schedule: `next_attempt_at` = +30s, +2m, +10m, +30m, +2h, +6h, +6h for attempts 1…7; attempt 8 → `failed` terminal; a `failed` row is never claimed again by the worker; total wall time of the test: seconds (clock is fake) | `I` | fake clock; bounded loop over the 8 attempts | `T-MEM-04/` | 5 |
| T-MEM-05 offline board | `C-OUTBOX-15`, `C-OUTBOX-16`, `C-OUTBOX-23`, `C-SCHEMA-13` | `MEMORY_DELIVERY=off`, handoffs with writes enqueued: board fully functional (claims, edits, handoffs all 200 — the outage degrades nothing); `GET /api/v1/outbox` lists the backlog newest-first; `GET /api/v1/outbox/summary` counts per status; nothing is failed, dropped or delivered; switching to `worker` mid-test delivers the whole backlog in `created_at` order, no replay command | `I` | FIFO-gated delivery observation | `T-MEM-05/` | 5 |
| T-MEM-06 stuck-worker recovery | `C-OUTBOX-5`, `C-OUTBOX-6`, `C-OUTBOX-17` | worker A claims row then is SIGKILLed mid-attempt (crashpoint `memory.post_adapter_write` **or** plain kill between claim and adapter return): row stays `processing`; fake clock past its 60 s lease; worker B (second process) reclaims → row goes `uncertain` first (`C-OUTBOX-6`), attempts already ≥1, then delivers; `uncertain` visible in `GET /outbox?status=uncertain` until resolved and is never reported as success before it is | `MP` — two worker processes on one file is exactly the contracted case | crashpoint + clock file | `T-MEM-06/` | 5 |
| T-MEM-07 dead-letter human retry | `C-OUTBOX-13`, `C-AUTH-7`, `C-ERR-14` | `POST /outbox/{id}/retry` on a `failed` row (human/orchestrator) → 200: `pending`, `attempts=0`, `next_attempt_at=NULL`, `last_error=NULL`, audited; retry of a `succeeded` row → `409 outbox_not_retryable`; agent retry → `403`; the retried row then delivers normally | `I` | fixed | `T-MEM-07/` | 5 |
| T-MEM-08 at-least-once + stable-id dedup | `C-OUTBOX-8`, `C-IDEM-5`, `C-OUTBOX-9` | engineer a double delivery (T-CRASH-c recovery): `calls.jsonl` shows the **same write id twice**, the mock returned the **same external_id** both times (dedupable at the far end), and the board ends with exactly one `succeeded` row and one `memory_delivered` audit — the board never asserted success it could not prove (`C-OUTBOX-2`) | `MP` | crashpoint + mock dedup | `T-MEM-08/` | 5 |
| T-MEM-09 search fallback | `C-OUTBOX-19`, `C-OUTBOX-20` | adapter without `Search` (wired as a non-Searcher mock) → `memory{action:"search"}` returns `501 search_unsupported` with `details.reason="adapter_has_no_search"` and `details.fallback="use memory_refs in handoffs"` — **never** an empty list; mock with corpus → results ordered, limit honoured; Searcher detection happens once at wiring (evidence: startup log line) | `I` | fixed corpus | `T-MEM-09/` | 5 |
| T-MEM-10 outbox visibility authz | `C-OUTBOX-23`, `C-AUTH-7` | agent reads outbox rows only for tickets it currently holds (its other-actor rows 404-filtered from the list — never leaked); human/orchestrator read all; `status=`/`ticket_id=` filters exact | `I` | fixed holdings | `T-MEM-10/` | 5 |
| T-MEM-11 delivery audit economy | `C-OUTBOX-24`, `C-AUDIT-4` | a row flapping pending→retryable→retryable→…→succeeded writes **exactly one** audit row (`memory_delivered`); a row ending `failed` writes exactly one (`memory_delivery_failed`); no per-attempt rows (attempt counter is the record) | `U` | fake clock drives attempts | `T-MEM-11/` | 5 |
