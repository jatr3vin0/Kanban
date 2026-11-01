# Ledgerboard — Phase 1 contracts

**Status:** working contract for Phase 2 (domain) and Phase 3 (transports). Not owner-ratified.
**Authority:** the owner's engineering spec is the requirement source. Where this document and
[`recon.md`](recon.md) disagree about what the code does today, `recon.md` wins and this document is
wrong — it is grounded in recon §2–§5 and §8. Where the spec is silent, the choice is marked
`[LEAD-DECIDED]` with one line of reasoning. Where a choice needs the owner, it is marked `[OWNER]`.

Every rule carries a stable ID (`C-<AREA>-<n>`). IDs are cited by `docs/test-plan.md`, the Phase 2/3
pull requests and QA. **Renaming or renumbering an ID is a contract revision, not an edit.** Adding a
rule at the end of a section is fine; reusing a retired ID for a different rule is not.

All SQL in this document was executed against the vendored driver on the real `0001_init` schema; the
transcript is [`docs/test-results/phase-1/sql-shape-probe.txt`](test-results/phase-1/sql-shape-probe.txt).
Two rules below exist *because* the probe caught the naive version failing (`C-CLAIM-7`, `C-CLAIM-13`).

---

## 0. Conventions that apply to every rule

| ID | Rule |
|---|---|
| `C-CONV-1` | **Two clocks, never compared.** Existing columns (`created_at`, `updated_at`, `ticket_events.created_at`, and every other column in `0001_init`) are `TEXT` RFC3339 **second precision** and stay untouched. Every new time column is `INTEGER` unix **milliseconds**. A query, comparison or Go expression that compares a TEXT time with an INTEGER time is a defect. Lease decisions read only INTEGER columns. |
| `C-CONV-2` | **Server time only.** `now_ms` is computed once per request in Go (`time.Now().UTC().UnixMilli()`), passed into every statement as a bound parameter, and never re-read inside SQL (`strftime('now')`, `datetime('now')`, `CURRENT_TIMESTAMP` are banned). Client-supplied times are never used for expiry, and no clock-skew allowance is applied. |
| `C-CONV-3` | **One transaction per mutation.** Every mutating operation runs inside `BEGIN IMMEDIATE` on the single writer connection (`_txlock=immediate`, `SetMaxOpenConns(1)`). Guard, mutation, audit rows, outbox rows and the idempotency record commit together or not at all. |
| `C-CONV-4` | **No read-then-write.** Every mutation is a single conditional `UPDATE` (or `INSERT`) whose `WHERE` encodes every guard, with the outcome decided by `RowsAffected()`. A `SELECT` may follow a 0-row result **for diagnosis only** (`C-CLAIM-12`); it may never decide whether to write. |
| `C-CONV-5` | **Enums are validated in Go, never by SQLite.** No `CHECK` constraints in `0002` (recon G17: `CHECK` needs a table rebuild, and existing rows may already violate it). A column typed `TEXT` with an enum meaning is validated at the service boundary; the store layer must reject unknown values itself, not rely on the handler. |
| `C-CONV-6` | **IDs** are 32 lowercase hex characters, matching the existing convention (recon R5 — not hyphenated UUIDs). New tables (`handoffs`, `outbox`, `idempotency_keys` rows) use the same generator. |
| `C-CONV-7` | **Every mutating response returns the whole entity** including `version` and the `claim` block (§1.4). A caller never has to re-read to learn its generation. |
| `C-CONV-8` | **Wire times.** INTEGER ms columns are serialized as RFC3339 UTC with exactly three fractional digits (`2026-10-30T23:08:22.180Z`). This differs from the existing second-precision fields by design: leases need sub-second resolution, and changing the existing fields would break `C-COMPAT-1`. |
| `C-CONV-9` | **Connection setup.** DSN `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_txlock=immediate`; one `*sql.DB` for writes with `SetMaxOpenConns(1)`, a separate read pool with `MaxOpenConns(4)`. Pragmas set by `db.Exec` are forbidden (recon H2: they reached 1 of 8 pooled connections). Verified: `busy_timeout=5000`, `foreign_keys=1` on the writer connection. |
| `C-CONV-10` | **Three timing knobs are contract values.** `LEDGERBOARD_LEASE_TTL_MS` (default `900000` — the spec's 15 minutes; floor `1000`), `LEDGERBOARD_SWEEP_INTERVAL_MS` (default `60000`) and `LEDGERBOARD_WORKER_POLL_MS` (default `1000`). Each is read once at startup and validated as a positive integer; **an invalid value is a startup error naming the variable**, never a silent fallback (`C-OUTBOX-14` is the same rule for `MEMORY_DELIVERY`, `C-CLAIM-3` for the claimable statuses). Precedence, highest first: a CLI flag if the binary has one for that knob, then the environment variable, then the default. The defaults above **are** the contract — tests assert them, and changing one is a contract revision. `:ttl_ms` in every statement in §2 is `LEDGERBOARD_LEASE_TTL_MS`, read at the moment a lease is issued or renewed; changing the knob never rewrites an existing `lease_expires_at`, so a shorter TTL takes effect at the next renewal and a longer one only for new leases. At the `1000` floor a lease can lapse while its holder is waiting out `busy_timeout`; that is legal and surfaces as `lease_expired` (`C-CLAIM-14`), which is why the floor is a test value and not a recommended one. |
| `C-CONV-11` | **Test-infrastructure seams are inert by default.** Two variables exist only so a test can name a moment: `LEDGERBOARD_CRASH_POINTS` (footnote below) and `LEDGERBOARD_CLOCK` with `LEDGERBOARD_CLOCK_FILE` (`real` by default; `fake` makes `now_ms` come from that file so a test can advance time without sleeping — it substitutes the Go source of `now_ms` and nothing else, so `C-CONV-2` still holds and every statement still receives `:now_ms` as a bound parameter). Unset, both are one atomic load and a branch on a path that already exists. They are **not** product behaviour: no contract outcome may depend on either being set, and a defect that reproduces only with one armed is a test bug, not a product bug. `[OWNER]` — **approved 2026-11-01 after a security check**: both seams are armed only by environment variables set when the server starts; nothing on the REST/MCP surface can arm or read either one; unarmed they are one atomic load and a branch; and an armed crashpoint acts on its own process only (write to the crash FIFO, fsync, SIGKILL itself). Phase 2 adds the negative controls: no client request can arm either seam, and with the variables unset no crashpoint fires. The alternative (real clock, real sleeps) makes the crash and expiry tests either slow or nondeterministic, and §21 requires both. |

**Footnote — the crash-point vocabulary (`C-CONV-11`).** The set is closed and contains eight points:
`claim.pre_commit`, `claim.post_commit`, `renew.pre_commit`, `renew.post_commit`,
`handoff.after_handoff_insert`, `handoff.pre_commit`, `memory.post_adapter_write`,
`memory.pre_ack_commit`. Each is one call site, `crashpoint.Hit("<name>")`, placed inside the
transaction whose atomicity it tests (`claim.*`/`renew.*` -> `C-CONV-3`, `C-AUDIT-4`, `C-IDEM-12`;
`handoff.*` -> `C-HANDOFF-14`, `C-IDEM-12`; `memory.*` -> `C-OUTBOX-2`, `C-OUTBOX-9`). Armed, the hook
writes `crash <name> <seq>` to the crash FIFO, fsyncs it, and `SIGKILL`s the process — never `os.Exit`
and never a panic, because a deferred commit or rollback is precisely what the test is testing. Unarmed
— the default, and always in a production run — no point can fire. The migration boundary is
deliberately **not** a crash point: that runner is vendored `golang-migrate`, so the crash is produced
by an external watcher killing the process at a named DDL statement, and `C-SCHEMA-19` is the behaviour
it must then observe. Adding a point is a test-plan change, not a contract change, but no point may be
added to a path with no rule behind it.

---

## 1. Additive schema changes — `0002_lease.{up,down}.sql`

### 1.1 The promise

| ID | Rule |
|---|---|
| `C-SCHEMA-1` | `0002_lease.up.sql` contains **only** `ALTER TABLE ... ADD COLUMN` against `tickets`, `CREATE INDEX`, and `CREATE TABLE`. No `DROP`, no `RENAME`, no table rebuild, no data `UPDATE`, no trigger (recon G9: `tickets` has four cascading FK children — `ticket_events`, `notes`, `tasks`, `ticket_relations` — and a rebuild would have to re-create them). |
| `C-SCHEMA-2` | Every added column is nullable **or** has a constant default, so the statement is legal on a populated table and existing rows get a defined value without a rewrite. Existing rows: `version = 1`, `lease_generation = 0`, `lease_renewals = 0`, every lease column `NULL`, `handoff_id NULL`. |
| `C-SCHEMA-3` | The migration is applied by the existing `golang-migrate` path (`internal/db/db.go`, `schema_migrations` table). No `PRAGMA user_version` logic is introduced (recon: `user_version` is 0 and unused). |
| `C-SCHEMA-4` | `0002_lease.down.sql` reverses the migration exactly: drop indexes, then columns in reverse order, then tables. It is **destructive by definition** (lease, handoff and outbox rows are lost) and is never invoked automatically — `golang-migrate` runs `Down` only on explicit request. Round-trip verified against a populated database: `tickets` returns to its 11 original columns, `foreign_key_check` is clean. `[LEAD-DECIDED]` — upstream ships down migrations, so the convention is kept; the destructiveness is documented rather than hidden. |
| `C-SCHEMA-19` | **A dirty migration fails closed.** If `schema_migrations.dirty = 1` — a previous run died between the version bump and the migration's own commit — the server **refuses to start**: it exits non-zero with one line naming the version and the remedy, before binding a listener and before serving any request. It never serves from a half-migrated database, never re-runs the migration automatically, never ignores the flag, and never clears it itself. Recovery is the operator's explicit `force` to a stated version (`golang-migrate force <version>`, or the binary's documented equivalent); that is the only action which resets the flag, and after it the migration is re-applied with the `0001` rows unchanged (`C-SCHEMA-2`). `[LEAD-DECIDED]` — the vendored driver already records dirtiness, so contracting the *reaction* is what stops fail-closed from being an implementation accident a later refactor removes. Placement note: appended here rather than after `C-SCHEMA-18` because it is a migration rule; IDs are stable names, not sequence numbers. |

### 1.2 `tickets` — added columns

```sql
ALTER TABLE tickets ADD COLUMN version           INTEGER NOT NULL DEFAULT 1;
ALTER TABLE tickets ADD COLUMN lease_holder      TEXT;
ALTER TABLE tickets ADD COLUMN lease_holder_type TEXT;
ALTER TABLE tickets ADD COLUMN lease_acquired_at INTEGER;
ALTER TABLE tickets ADD COLUMN lease_expires_at  INTEGER;
ALTER TABLE tickets ADD COLUMN lease_generation  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tickets ADD COLUMN lease_renewals    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tickets ADD COLUMN handoff_id        TEXT;
```

| Column | Type | Unit / meaning | Written by |
|---|---|---|---|
| `version` | INTEGER, NOT NULL, default 1 | Optimistic-concurrency counter. Starts at 1, `+1` per accepted state-changing mutation (§3). | every mutation |
| `lease_holder` | TEXT, nullable | Actor id, e.g. `agent:builder-1`. `NULL` = unclaimed. **Not** the same as `assignee`, which stays a free-text label and is never used for ownership. | claim/reassign/release/expire/handoff |
| `lease_holder_type` | TEXT, nullable | `human` \| `agent` \| `orchestrator` \| `system`, validated in Go (`C-CONV-5`). | same as holder |
| `lease_acquired_at` | INTEGER, nullable | unix ms, server time at the claim that issued the current generation. | claim/reassign/handoff |
| `lease_expires_at` | INTEGER, nullable | unix ms. `> now_ms` = live; `<= now_ms` = lapsed. Never compared to a TEXT column. | claim/renew/reassign/handoff |
| `lease_generation` | INTEGER, NOT NULL, default 0 | Fencing token. `0` = never issued, and is **never** a valid token. Monotonic per ticket (§3). | every ownership transition |
| `lease_renewals` | INTEGER, NOT NULL, default 0 | Renewals in the current generation; reset to 0 on every new generation. Diagnostics and the UI's "slow agent" signal, never a limit on renewal. | renew |
| `handoff_id` | TEXT, nullable | The most recently accepted handoff for this ticket. Deliberately **no FK**: the referenced handoff is cascade-deleted with the ticket anyway, so the constraint buys nothing and would have to be dropped on rollback (`C-SCHEMA-4`). Validated in Go. | handoff accept |

| ID | Rule |
|---|---|
| `C-SCHEMA-5` | `assignee` keeps its current meaning (free-text label, `''` = unassigned) and is **never** read to decide ownership. `lease_holder` is the only ownership fact. A UI that shows both must label them differently. |
| `C-SCHEMA-6` | `lease_generation = 0` means "never claimed". Any claim-protected operation presented with generation `0` fails with `lease_generation_mismatch`. |
| `C-SCHEMA-7` | No index is placed on any column that `C-SCHEMA-4` must drop, except the two indexes in §1.3, which the down migration drops first. |

### 1.3 Indexes

```sql
CREATE INDEX tickets_claimable    ON tickets(board_id, status, lease_expires_at);
CREATE INDEX tickets_lease_holder ON tickets(lease_holder) WHERE lease_holder IS NOT NULL;
```

| ID | Rule |
|---|---|
| `C-SCHEMA-8` | `tickets_claimable` serves `claim_next` (board + claimable status + lease expiry) and the board ticket list. `tickets_lease_holder` is partial so it stays small; it serves "what am I holding" and the expiry sweep. No other index is added in this phase — recon confirms `tickets` has no index at all today and none is needed for correctness. |

### 1.4 `handoffs`

```sql
CREATE TABLE handoffs (
    id                TEXT PRIMARY KEY,
    ticket_id         TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    board_id          TEXT NOT NULL REFERENCES boards(id)  ON DELETE CASCADE,
    schema_version    INTEGER NOT NULL DEFAULT 1,
    from_actor        TEXT NOT NULL,
    from_actor_type   TEXT NOT NULL,
    to_actor          TEXT,
    generation        INTEGER NOT NULL,
    summary           TEXT NOT NULL,
    done_json         TEXT NOT NULL DEFAULT '[]',
    next_json         TEXT NOT NULL DEFAULT '[]',
    blockers_json     TEXT NOT NULL DEFAULT '[]',
    artifacts_json    TEXT NOT NULL DEFAULT '[]',
    memory_writes_json TEXT NOT NULL DEFAULT '[]',
    memory_refs_json  TEXT NOT NULL DEFAULT '[]',
    status            TEXT NOT NULL DEFAULT 'offered',
    supersedes        TEXT,
    accepted_by       TEXT,
    accepted_at       INTEGER,
    created_at        INTEGER NOT NULL,
    request_id        TEXT NOT NULL DEFAULT '',
    checksum          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX handoffs_ticket_created ON handoffs(ticket_id, created_at DESC);
CREATE INDEX handoffs_status         ON handoffs(status, created_at DESC);
```

| ID | Rule |
|---|---|
| `C-SCHEMA-9` | `handoffs` rows are **append-only**: no `UPDATE` except the single acceptance transition (`status`, `accepted_by`, `accepted_at`) and the supersession transition (`status`). There is no `DELETE` path in the product. Immutability is enforced by the service layer having no other statement. |
| `C-SCHEMA-10` | The typed columns are the storage form; the v1 JSON document is rebuilt from them on read (§5). `checksum` is the lowercase hex SHA-256 of the canonical JSON of the **checksummed field set** defined in `C-HANDOFF-9` — the creation-time fields only, which is why it survives the accept and supersession transitions unchanged. Any drift in a creation-time field is therefore detectable and idempotent replays compare cheaply. |
| `C-SCHEMA-11` | `generation` records the fencing generation the author held when the handoff was written — evidence of authority, not a live token. `to_actor NULL` means "offered to whoever claims next". |
| `C-SCHEMA-12` | `supersedes` references another handoff id with no FK (a correction is a new row; the chain is walked in Go and must not cycle — a self-reference is rejected). |

### 1.5 `outbox`

```sql
CREATE TABLE outbox (
    id               TEXT PRIMARY KEY,
    ticket_id        TEXT NOT NULL,
    handoff_id       TEXT,
    board_id         TEXT NOT NULL DEFAULT '',
    actor            TEXT NOT NULL,
    actor_type       TEXT NOT NULL,
    memory_type      TEXT NOT NULL,
    content          TEXT NOT NULL,
    confidence       REAL NOT NULL DEFAULT 1.0,
    tags_json        TEXT NOT NULL DEFAULT '[]',
    status           TEXT NOT NULL DEFAULT 'pending',
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  INTEGER,
    lease_owner      TEXT,
    lease_expires_at INTEGER,
    external_id      TEXT,
    last_error       TEXT,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    delivered_at     INTEGER,
    request_id       TEXT NOT NULL DEFAULT '',
    idempotency_key  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_claimable ON outbox(status, next_attempt_at);
CREATE INDEX outbox_ticket    ON outbox(ticket_id);
CREATE INDEX outbox_handoff   ON outbox(handoff_id);
CREATE INDEX outbox_lease     ON outbox(status, lease_expires_at);
```

| ID | Rule |
|---|---|
| `C-SCHEMA-13` | `outbox.ticket_id` and `outbox.handoff_id` carry **no FK**, deliberately. A memory write is an obligation to the operator's memory system, not a child of the ticket: `ON DELETE CASCADE` would silently destroy an undelivered write when a ticket is deleted. Verified: after deleting a ticket, its handoffs are gone and its outbox rows remain. |
| `C-SCHEMA-14` | `id` is the stable write id (32 hex). It is generated once, at enqueue, inside the handoff transaction, and is the idempotency key for every delivery attempt (§6.6). It never changes. |
| `C-SCHEMA-15` | `memory_type` ∈ `fact`, `decision`, `requirement`, `finding`, `preference`, `continuation`, `lesson`; `confidence` ∈ [0.0, 1.0]; `tags_json` is a JSON array of ≤ 16 strings of ≤ 64 chars. Validated in Go at enqueue (`C-CONV-5`). |
| `C-SCHEMA-16` | `last_error` is ≤ 500 chars, scrubbed of anything matching a credential pattern (`C-AUDIT-6`), and is a message plus a code — never a stack trace, never a request header. |

### 1.6 `idempotency_keys`

```sql
CREATE TABLE idempotency_keys (
    actor           TEXT NOT NULL,
    operation       TEXT NOT NULL,
    key             TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL,
    response_status INTEGER,
    response_body   TEXT,
    entity_id       TEXT,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    PRIMARY KEY (actor, operation, key)
);
CREATE INDEX idempotency_expires ON idempotency_keys(expires_at);
```

| ID | Rule |
|---|---|
| `C-SCHEMA-17` | The primary key is the idempotency scope: `(actor, operation, key)` (§4.1). The unique index is the enforcement — the duplicate-key error is the conflict signal, not a prior `SELECT`. |
| `C-SCHEMA-18` | `response_body` stores the **exact bytes** returned to the original caller (JSON text) so a replay is byte-identical. `entity_id` duplicates the created entity id for convenience and for `Location` reconstruction. |

### 1.7 Audit — extending `ticket_events`

`ticket_events` is not altered: `id`, `ticket_id`, `type`, `actor`, `payload`, `created_at` all keep
their shape (recon §5.1). The extension is **new `type` values plus a populated `actor`**.
`[LEAD-DECIDED]` — one timeline in the UI beats two audit tables with two vocabularies (recon D8).

| ID | Rule |
|---|---|
| `C-AUDIT-1` | `actor` is now written on every new row in the form `<type>:<id>` — `human:alice`, `agent:builder-1`, `orchestrator:fleet-1`, `system:sweeper`. Pre-existing rows with `actor = ''` mean "unknown" and must render as such, never as a guess. |
| `C-AUDIT-2` | Every new payload is a JSON object that **must** contain `request_id` (the request's correlation id, `''` when the caller supplied none) and, for ownership events, `generation`. |
| `C-AUDIT-3` | New `type` values and payload shapes are exactly those in §1.8. A type not listed there is a contract violation; the UI timeline must tolerate unknown types (render the raw payload). |
| `C-AUDIT-4` | Audit rows are written **inside the same transaction** as the mutation they describe. There is no code path that writes an audit row for a mutation that rolled back, and none that mutates without an audit row (recon §4.4 shows the current code doing the opposite). |
| `C-AUDIT-5` | Audit is append-only: no `UPDATE` or `DELETE` on `ticket_events` anywhere in the product. The single exception is the existing FK cascade when the ticket itself is deleted (see `C-AUDIT-9`). |
| `C-AUDIT-6` | **No secrets.** Payloads never contain `Authorization` headers, tokens, adapter credentials, full memory contents beyond the handoff's own `memory_writes`, or connection strings. A payload that would carry one is replaced by `{"redacted": true}`. |
| `C-AUDIT-7` | Reads of `ticket_events` order by `created_at ASC, rowid ASC`. `created_at` is second-precision (recon §4.6), so `rowid` is the only stable tiebreaker; ordering by `created_at` alone is a defect. |
| `C-AUDIT-8` | `ticket_events.created_at` stays TEXT RFC3339 seconds (`C-CONV-1`). Lease *values* inside payloads are unix ms integers, because they are data, not the event timestamp. |
| `C-AUDIT-9` | **Known gap, carried forward from recon G10/R4:** ticket deletion is still not audited — a `deleted` row would cascade away with the ticket. Board and epic mutations are likewise unaudited, because `ticket_events` has a `NOT NULL` FK to `tickets` and cannot hold them. A global `audit_events` table is deferred out of v1; this is a documented gap against spec §13, not an oversight. `[LEAD-DECIDED]` — the ownership engine is entirely ticket-scoped, so the deferral costs nothing the spec's §13 is actually protecting. |

### 1.8 Audit payloads per event type

All payloads are JSON objects. `generation` is the **post**-transition generation.

| `type` | When | Payload |
|---|---|---|
| `claimed` | a claim or `claim_next` succeeds | `{"request_id","generation","actor_type","lease_expires_at":<ms>,"previous_holder":<string\|null>,"previous_generation":<int>,"source":"claim"\|"claim_next"}` |
| `claim_renewed` | a renewal succeeds | `{"request_id","generation","previous_expires_at":<ms>,"lease_expires_at":<ms>,"renewals":<int>}` |
| `claim_released` | a holder's own release (any actor type that can hold the lease), expiry sweep, or a status change that ends the claim | `{"request_id","generation","previous_holder","actor_type","status_from","status_to","reason":"release"\|"expiry"\|"status"}` |
| `claim_reassigned` | human/orchestrator reassign (incl. takeover of a live lease) | `{"request_id","generation","previous_generation","from_holder":<string\|null>,"to_holder","to_holder_type","lease_expires_at":<ms>,"override":<bool>}` |
| `claim_revoked` | human/orchestrator revoke (claim dies, ticket stays as it is) | `{"request_id","generation","previous_holder","reason"}` |
| `handoff_created` | handoff accepted at creation time | `{"request_id","handoff_id","generation","schema_version":1,"to_actor":<string\|null>,"supersedes":<string\|null>}` |
| `handoff_accepted` | a second actor accepts an offered handoff and takes ownership | `{"request_id","handoff_id","generation","previous_generation","from_holder","to_holder"}` |
| `handoff_superseded` | a correction supersedes an earlier handoff | `{"request_id","handoff_id","superseded_by"}` |
| `outbox_enqueued` | handoff transaction creates outbox rows | `{"request_id","handoff_id","count":<int>,"write_ids":[<string>...]}` |
| `memory_delivered` | an outbox row reaches `succeeded` | `{"request_id","write_id","external_id","attempts":<int>}` |
| `memory_delivery_failed` | an outbox row reaches `failed` (dead-letter) | `{"request_id","write_id","attempts":<int>,"error_code"}` |
| `unblocked` | human/orchestrator moves a ticket out of `blocked` | `{"request_id","from":"blocked","to":"todo"\|"in_progress"\|"done","previous_generation":<int>,"generation":<int>}` |

Existing types (`created`, `moved`, `edited`, `commented`, `comment_edited`, `task_added`,
`task_updated`, `task_deleted`, `relation_added`, `relation_removed`) keep their payload shapes
(recon §5.1) and gain only `request_id`, which old readers ignore (`C-COMPAT-5`).

---

## 2. Claim state machine

### 2.1 States

Ownership state is **derived** from the row, never stored as its own column — a stored state machine
would be a second source of truth that can disagree with the lease fields.

| State | Predicate (with `now = now_ms`) |
|---|---|
| `UNCLAIMED` | `lease_holder IS NULL` |
| `ACTIVE` | `lease_holder IS NOT NULL AND lease_expires_at > now` |
| `LAPSED` | `lease_holder IS NOT NULL AND lease_expires_at <= now` |

Ticket `status` (`todo` / `in_progress` / `blocked` / `done`) is orthogonal and unchanged in
vocabulary. `blocked` is a first-class status (recon D1 — the earlier design docs did not model it).

| ID | Rule |
|---|---|
| `C-CLAIM-1` | `LAPSED` is not a stored transition. A lease is lapsed the instant `now >= lease_expires_at`; no sweeper is required for correctness, and the claim guard treats a lapsed lease as free. The sweeper (§2.6) exists for UI/status hygiene only. |
| `C-CLAIM-2` | **A `blocked` ticket is not claimable.** Claimability requires a claimable status (`todo` by default, `in_progress` when its lease has lapsed or it has no holder) and no unfinished blocking dependency. To make a blocked ticket claimable, a human/orchestrator must unblock it (`C-CLAIM-14`) — that is the spec's §5.5 "unblock" override, and it is audited as `unblocked`. An agent may set `blocked` on a ticket it holds; only `human`/`orchestrator` may clear it. |
| `C-CLAIM-3` | Claimable statuses come from config `LEDGERBOARD_CLAIMABLE_STATUSES` (default `todo`, comma-separated; `done` and `blocked` are never accepted in the list — the config validator rejects them). `[LEAD-DECIDED]` — the spec allows a config override; forbidding `done`/`blocked` keeps the override from creating an unclaimable-by-design ticket. |

### 2.2 Transitions

| # | From → To | Operation | Actor | Guard | Generation | Version | Result |
|---|---|---|---|---|---|---|---|
| T1 | `UNCLAIMED`/`LAPSED` → `ACTIVE` | `claim` | agent, orchestrator | status claimable (`C-CLAIM-7`), no unfinished blocker (`C-CLAIM-8`), lease free | `+1` | `+1` | 200 + ticket |
| T2 | `ACTIVE` → `ACTIVE` | `renew_claim` | current holder only | holder + generation match, lease still live (`C-CLAIM-13`) | unchanged | unchanged | 200 + ticket |
| T3 | `ACTIVE`/`LAPSED` → `UNCLAIMED` | `release_claim` | holder only — whoever holds the lease releases its own claim, whatever its actor type: agents and orchestrators take a lease by claiming (T1), a human by handoff accept (`C-HANDOFF-16`, T7) or reassign to self (T8). A caller that is not the holder gets `409 not_claim_holder` (`C-CLAIM-18`). A human or orchestrator taking work away from *someone else* does not use `release`: it uses `reassign` (T5) or `revoke` (T6), which need no holder guard (`C-GUARD-7`, `C-GUARD-8`) | holder + generation match (holder path) — **no liveness check**, so a lapsed holder may release (`C-CLAIM-18`) | `+1` | `+1` | 200 + ticket |
| T4 | `ACTIVE` → `LAPSED` → `UNCLAIMED` | expiry sweep | system | `lease_expires_at <= now` | `+1` | `+1` | audit only |
| T5 | any → `ACTIVE` | `reassign` | human, orchestrator | `override` required over a live lease (`C-GUARD-7`); the new generation kills the old holder's token (`C-GUARD-8`) | `+1` | `+1` | 200 + ticket |
| T6 | `ACTIVE`/`LAPSED` → `UNCLAIMED` | `revoke` | human, orchestrator | ticket exists | `+1` | `+1` | 200 + ticket |
| T7 | `ACTIVE` → `ACTIVE` | `handoff` accept | current holder → `to_actor`, or claimer of an offered handoff | holder + generation match, or offered handoff | `+1` | `+1` | 200 + handoff |
| T8 | `ACTIVE` → `ACTIVE` | human takeover (reassign to self) | human | same as T5 | `+1` | `+1` | 200 + ticket |
| T9 | `ACTIVE`/`LAPSED` → `UNCLAIMED` | status change to `done` or `blocked`, or leaving `blocked`, while claimed | any | status transition valid | `+1` | `+1` | 200 + ticket |

| ID | Rule |
|---|---|
| `C-CLAIM-4` | There is no transition that leaves a `LAPSED` lease with its old holder **and** the old generation. Either a new generation is issued (claim/reassign) or the holder is cleared (release/sweep/revoke). |
| `C-CLAIM-5` | A claim never *steals* an `ACTIVE` lease. Contention over a live lease is resolved by `409 already_claimed`; the only way to take a live lease is a human/orchestrator `reassign` with `override: true`. |
| `C-CLAIM-6` | Claiming sets `status = 'in_progress'` in the same statement. Releasing or sweeping sets `status = 'todo'` **only if** it is currently `in_progress`; `done` and `blocked` are left alone (a human who marked it done does not want it back in `todo`). |

### 2.3 `claim` — the exact statement

```sql
-- inside BEGIN IMMEDIATE; :now_ms computed once in Go
UPDATE tickets
   SET status = 'in_progress',
       lease_holder = :actor_id,
       lease_holder_type = :actor_type,
       lease_acquired_at = :now_ms,
       lease_expires_at = :now_ms + :ttl_ms,
       lease_generation = lease_generation + 1,
       lease_renewals = 0,
       version = version + 1,
       updated_at = :updated_at_text      -- RFC3339 seconds, C-CONV-1
 WHERE id = :ticket_id
   AND (lease_holder IS NULL OR lease_expires_at <= :now_ms)
   AND ( status IN (:claimable_statuses)                                  -- default ('todo')
         OR (status = 'in_progress' AND (lease_holder IS NULL OR lease_expires_at <= :now_ms)) )
   AND NOT EXISTS (
       SELECT 1 FROM ticket_relations r
         JOIN tickets b ON b.id = r.from_ticket_id
        WHERE r.to_ticket_id = tickets.id
          AND r.kind = 'blocks'
          AND b.status <> 'done');
```

| ID | Rule |
|---|---|
| `C-CLAIM-7` | The `status` predicate is **three-way**, not `status = 'todo'`. A lapsed lease leaves `status = 'in_progress'` (nobody swept it), so a `todo`-only guard makes the ticket unclaimable until a sweeper runs — which violates `C-CLAIM-1`. This exact defect was found by the probe: with `status = 'todo'`, a claim over an expired lease returned 0 rows. The third arm (`in_progress` with no holder) covers a ticket a human moved to `in_progress` without claiming it, which would otherwise be stuck forever. |
| `C-CLAIM-8` | A blocker counts as unfinished unless its `status = 'done'`. `blocked` blockers are unfinished. There is no cycle detection in `ticket_relations` (recon §7) and this contract does not add one — a cycle blocks both tickets, which is visible and fixable by a human, whereas a naive cycle check in a hot path would be a read-before-write (`C-CONV-4`). `[LEAD-DECIDED]` |
| `C-CLAIM-9` | `RowsAffected() == 1` is success; the response is read back **in the same transaction** and returned with `version` and `claim`. `RowsAffected()` can never be `> 1` (single-row `id` predicate); if it is, that is a `500 internal` and a bug. |
| `C-CLAIM-10` | On success the transaction also writes the `claimed` audit row (`§1.8`) and publishes the SSE event. Both are inside the transaction's scope for the audit row; the SSE publish happens after commit (`C-COMPAT-8`). |
| `C-CLAIM-11` | A claim whose actor is `human` is rejected with `403 forbidden` — humans do not take leases, they reassign (`C-AUTH-4`). `system` may claim only via the internal sweeper/worker path, never over a transport. |

### 2.4 `claim_next` — one statement, no read-then-write

Same `SET` clause as `C-CLAIM-7`, with the id selected in the same statement:

```sql
UPDATE tickets
   SET status = 'in_progress', lease_holder = :actor_id, lease_holder_type = :actor_type,
       lease_acquired_at = :now_ms, lease_expires_at = :now_ms + :ttl_ms,
       lease_generation = lease_generation + 1, lease_renewals = 0,
       version = version + 1, updated_at = :updated_at_text
 WHERE id = (
   SELECT id FROM tickets
    WHERE board_id = :board_id
      AND (lease_holder IS NULL OR lease_expires_at <= :now_ms)
      AND ( status IN (:claimable_statuses)
            OR (status = 'in_progress' AND (lease_holder IS NULL OR lease_expires_at <= :now_ms)) )
      AND NOT EXISTS (
          SELECT 1 FROM ticket_relations r
            JOIN tickets b ON b.id = r.from_ticket_id
           WHERE r.to_ticket_id = tickets.id AND r.kind = 'blocks' AND b.status <> 'done')
    ORDER BY CASE priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1
                           WHEN 'medium'   THEN 2 ELSE 3 END,
             created_at ASC, id ASC
    LIMIT 1)
RETURNING id, lease_generation, version;
```

| ID | Rule |
|---|---|
| `C-CLAIM-12` | `claim_next` is an **extension** beyond spec §5.3's four operations. `[LEAD-DECIDED]` — "agents discover work" is the product's promise, and this is one statement reusing the claim guard, not new machinery. It must be implemented exactly as above (priority weight reuses the existing `CASE` from `ListTickets`/`ReadyTickets`, recon D11 — `priority` is TEXT, so a numeric sort is wrong). If Phase 2 runs short, dropping `claim_next` is a legitimate scope cut; changing its SQL is not. |
| `C-CLAIM-13` | Zero rows from `claim_next` means "nothing claimable on this board" → `404 not_found` with `details.reason = "no_claimable_ticket"`, not an error state to retry. |

### 2.5 `renew`, `release`, `revoke`

```sql
-- renew: holder + generation + still-live lease, all in the WHERE
UPDATE tickets
   SET lease_expires_at = :now_ms + :ttl_ms,
       lease_renewals = lease_renewals + 1,
       updated_at = :updated_at_text
 WHERE id = :ticket_id
   AND lease_holder = :actor_id
   AND lease_generation = :generation
   AND lease_expires_at > :now_ms
   AND status = 'in_progress';

-- release (holder path)
UPDATE tickets
   SET status = CASE WHEN status = 'in_progress' THEN 'todo' ELSE status END,
       lease_holder = NULL, lease_holder_type = NULL,
       lease_acquired_at = NULL, lease_expires_at = NULL, lease_renewals = 0,
       lease_generation = lease_generation + 1,
       version = version + 1, updated_at = :updated_at_text
 WHERE id = :ticket_id
   AND lease_holder = :actor_id
   AND lease_generation = :generation;

-- revoke (human/orchestrator, no holder guard: it clears whatever is there)
UPDATE tickets
   SET status = CASE WHEN status = 'in_progress' THEN 'todo' ELSE status END,
       lease_holder = NULL, lease_holder_type = NULL,
       lease_acquired_at = NULL, lease_expires_at = NULL, lease_renewals = 0,
       lease_generation = lease_generation + 1,
       version = version + 1, updated_at = :updated_at_text
 WHERE id = :ticket_id
   AND lease_holder IS NOT NULL;
```

The absence of `lease_expires_at > :now_ms` in the **release** statement (and its presence in `renew`)
is `C-CLAIM-18`, not an oversight: release has no clock in its guard, renew has one.

| ID | Rule |
|---|---|
| `C-CLAIM-14` | **Renewal never resurrects a lapsed lease.** `lease_expires_at > :now_ms` is part of the `WHERE`. A lapsed holder gets `409 lease_expired` and must re-claim (competing with everyone else). `[LEAD-DECIDED]` — allowing an expired holder to renew would let a zombie process reclaim ownership after the ticket was legitimately swept, and would make `status` disagree with the lease. Guidance for slow agents: renew at 50 % of the TTL (7.5 min), not at expiry. |
| `C-CLAIM-15` | Renewal does **not** change `version` and does **not** change `lease_generation`. It is a heartbeat, not a state change: bumping `version` on every renewal would make an in-flight editor's `expected_version` fail for no reason. `[LEAD-DECIDED]` |
| `C-CLAIM-16` | `renew` requires `lease_holder` **and** `lease_generation` to match — the generation alone is never sufficient, because generations are per-ticket and would otherwise let actor B reuse actor A's number on the same ticket. |
| `C-CLAIM-17` | Release and revoke bump the generation so that a stale claim-protected write from the old holder cannot land on the now-unclaimed ticket (`§3.4`). |
| `C-CLAIM-18` | **`release` has no expiry guard; `lease_expired` belongs to `renew` alone.** A release whose `lease_holder` **and** `lease_generation` both match succeeds whether the lease is live or lapsed — the statement above is the whole guard and it contains no clock. A caller that is not the current holder (never was, or was superseded) gets `409 not_claim_holder`; a caller whose holder matches but whose generation is stale gets `409 lease_generation_mismatch`; `409 lease_expired` is returned by `renew` only (`C-CLAIM-14`). `[LEAD-DECIDED]` — release *surrenders* authority instead of extending it, so the danger `C-CLAIM-14` guards against does not exist here: every new claim issues a higher generation (`C-INV-2`, `C-FENCE-9`), so a stale release can never end anyone else's lease, while refusing a lapsed holder would leave a crashed agent with no way to tidy the row it left behind. The asymmetry with `renew` is deliberate; it is not to be "fixed" by adding `lease_expires_at > :now_ms` to the release statement. |

### 2.6 Expiry sweep

```sql
-- one statement, per board or global; ids come back for the audit rows
UPDATE tickets
   SET status = CASE WHEN status = 'in_progress' THEN 'todo' ELSE status END,
       lease_holder = NULL, lease_holder_type = NULL,
       lease_acquired_at = NULL, lease_expires_at = NULL, lease_renewals = 0,
       lease_generation = lease_generation + 1,
       version = version + 1, updated_at = :updated_at_text
 WHERE lease_holder IS NOT NULL
   AND lease_expires_at <= :now_ms
RETURNING id, version, lease_generation;
```

| ID | Rule |
|---|---|
| `C-CLAIM-19` | The sweep is **optional for correctness** (`C-CLAIM-1`) and idempotent: it can run every minute or never. It must be single-statement with `RETURNING` so the ids and post-values for the audit rows come from the same write, not a prior `SELECT`. |
| `C-CLAIM-20` | The sweep is performed by the `system` actor and runs on a ticker of `LEDGERBOARD_SWEEP_INTERVAL_MS` (`C-CONV-10`; default `60000`, i.e. 60 s) inside the server process (a goroutine), plus once on startup. There is no external cron and no second binary. |
| `C-CLAIM-21` | A sweep must not fight a concurrent renew: the `WHERE lease_expires_at <= :now_ms` is evaluated by SQLite under the write lock, so either the renew commits first (and the lease is live, so the sweep skips it) or the sweep commits first (and the renew's own `lease_expires_at > :now_ms` guard then fails with `409 lease_expired`). Both outcomes are correct; a `500` is not. |

### 2.7 The 0-row diagnosis table

Every mutating statement above can affect 0 rows. `C-CONV-4` forbids deciding from a prior read, so the
diagnosis happens **after** the 0-row result, inside the same `BEGIN IMMEDIATE` transaction (the write
lock is still held, so the read is consistent with the statement that just failed).

| Operation | 0 rows means (checked in this order) | Error | REST | Details carried |
|---|---|---|---|---|
| `claim` | ticket absent | `not_found` | 404 | `ticket_id` |
| | status not claimable (`done`, `blocked`, or not in `LEDGERBOARD_CLAIMABLE_STATUSES`) | `status_not_claimable` | 409 | `ticket_id`, `status`, `claimable_statuses` |
| | unfinished blocker | `dependency_blocked` | 409 | `ticket_id`, `blocking_ticket_ids[]`, `blocking_statuses[]` |
| | live lease held by someone else | `already_claimed` | 409 | `ticket_id`, `status`, `version`, `holder`, `holder_type`, `lease_expires_at`, `lease_generation` |
| `claim_next` | no claimable ticket on board | `not_found` | 404 | `board_id`, `reason:"no_claimable_ticket"` |
| `renew` | ticket absent | `not_found` | 404 | `ticket_id` |
| | not the holder | `not_claim_holder` | 409 | `ticket_id`, `holder`, `holder_type` |
| | holder but generation stale | `lease_generation_mismatch` | 409 | `ticket_id`, `expected_generation`, `current_generation` |
| | holder and generation match but lease lapsed | `lease_expired` | 409 | `ticket_id`, `lease_expires_at`, `current_generation` |
| | holder, generation, live lease, but status ≠ `in_progress` † | `status_not_claimable` | 409 | `ticket_id`, `status` |
| `release` | ticket absent | `not_found` | 404 | `ticket_id` |
| | not the holder (never was, or someone else holds it) | `not_claim_holder` | 409 | `ticket_id`, `holder` |
| | holder but generation stale (superseded) | `lease_generation_mismatch` | 409 | `ticket_id`, `expected_generation`, `current_generation` |
| `revoke` | ticket absent | `not_found` | 404 | `ticket_id` |
| | no claim to revoke | `not_claimed` | 409 | `ticket_id` |
| `reassign` | ticket absent | `not_found` | 404 | `ticket_id` |
| | live lease and `override` not set | `already_claimed` | 409 | as above + `override_required:true` |
| `handoff` accept | ticket absent | `not_found` | 404 | `ticket_id` |
| | caller is not holder (or generation stale) | `not_claim_holder` / `lease_generation_mismatch` | 409 | as above |
| | handoff not `offered`, or `to_actor` is someone else | `handoff_not_acceptable` | 409 | `handoff_id`, `status`, `to_actor` |
| content update | ticket absent | `not_found` | 404 | `ticket_id` |
| | `expected_version` ≠ current | `version_conflict` | 409 | `ticket_id`, `expected_version`, `current_version` |
| | claim-protected write by non-holder | `not_claim_holder` | 409 | `ticket_id`, `holder` |
| | claim-protected write with stale generation | `lease_generation_mismatch` | 409 | `ticket_id`, `expected_generation`, `current_generation` |

† **Defensive row: unreachable in a database the contract can produce.** `C-INV-4` says a live lease
implies `status = 'in_progress'`, and every statement that ends a claim clears the holder in the same
row update, so no sequence of contract operations can reach this row's precondition. It stays because a
`500` (`C-CLAIM-23`) is a worse answer than a truthful 409 if an invariant is ever broken, and because
the store must not crash on a row a direct `UPDATE` produced. QA covers it with a direct-SQL fixture,
never through a transport (`C-CLAIM-25`).

Two operations deliberately have **no** `lease_expired` row, and the absence is normative: `release` —
a lapsed holder with the current generation **succeeds**, because the release statement has no clock in
it (`C-CLAIM-18`) — and `revoke`, which has no holder guard at all. A diagnosis table that grows a
`lease_expired` row for `release` has contradicted `C-CLAIM-18`.

| ID | Rule |
|---|---|
| `C-CLAIM-22` | The diagnosis order in the table is normative: the **first** matching row is the error returned. "Ticket absent" is always first (404 before 409). |
| `C-CLAIM-23` | If the diagnosis SELECT finds nothing to explain the 0 rows, that is a `500 internal` and a logged defect — it means the guard and the diagnosis disagree, which must never happen silently. |
| `C-CLAIM-24` | A 0-row result is never retried automatically inside the store. Retry policy belongs to the caller (the agent), and only for `database_busy`. |
| `C-CLAIM-25` | The † row is defensive-only and must not be cited as a reachable outcome. The **reachable** path to `409 lease_generation_mismatch` on a content edit is a generation bump that leaves `lease_holder` unchanged, so the guard's holder arm (`C-GUARD-2`) matches and its generation arm does not: (a) a human/orchestrator `reassign` to the ticket's own current holder (T8), or (b) the same actor releasing and re-claiming, so a request still in flight from before carries a generation that is no longer current. Every other generation change also changes the holder (§3.3, `C-INV-2`), so those surface as `not_claim_holder` or `already_claimed` instead. Tests use (a) deliberately. |

### 2.8 Invariants

| ID | Invariant | How it is enforced |
|---|---|---|
| `C-INV-1` | At most one active holder per ticket. | Ownership is a column on one row; there is no second place to record a holder. |
| `C-INV-2` | `lease_generation` is strictly monotonic per ticket: it only ever increases, by exactly 1, on an ownership transition; it is never reset and never decremented. | Every transition statement contains `lease_generation = lease_generation + 1`; no statement writes a literal generation. |
| `C-INV-3` | A ticket with `lease_holder IS NULL` has `lease_expires_at`, `lease_acquired_at` and `lease_holder_type` all `NULL` and `lease_renewals = 0`. | The release/sweep/revoke statements set all five together in one row update; a partial clear is a contract violation. |
| `C-INV-4` | A live lease always implies `status = 'in_progress'`. The only way to move a claimed ticket out of `in_progress` is to end the claim in the same statement (T9: a status change to `done`/`blocked` releases the lease). | Claim sets `in_progress`; release/sweep/T9 clear the holder in the same row update; nothing else writes `status` without either the ownership guard or an explicit release. |
| `C-INV-5` | No mutation is a read-then-write. | `C-CONV-4`; reviewable by grepping for `SELECT` before `UPDATE` in the store package. |
| `C-INV-6` | `version` increases by exactly 1 on every accepted mutation that changes client-visible state, and by 0 on renewal. | The `SET` clause of each statement. |
| `C-INV-7` | Every lease comparison uses INTEGER ms and `now_ms` from the server. | `C-CONV-1`, `C-CONV-2`. |
| `C-INV-8` | The audit row for a transition carries the post-transition generation, and no audit row exists for a mutation that rolled back. | `C-AUDIT-4`; same transaction. |

---

## 3. Fencing and version rules

### 3.1 Two different numbers

| | `lease_generation` | `version` |
|---|---|---|
| Question answered | "Is my *authority* still valid?" | "Is my *copy* of the ticket still current?" |
| Scope | Ownership only | Whole ticket row |
| Issued by | claim, reassign, release, revoke, sweep, handoff accept | every state-changing mutation |
| Increments on | ownership transitions only | content/status/ownership changes; **not** renewal |
| Sent as | `generation` on claim-protected writes | `expected_version` on any versioned write |
| On mismatch | `409 lease_generation_mismatch` | `409 version_conflict` |
| Retry? | No — re-read and decide, the ticket may be someone else's | Yes — re-read, re-apply, resend |

| ID | Rule |
|---|---|
| `C-FENCE-1` | `lease_generation` is the fencing token. It is a plain counter, not a secret: it is useless without the matching `lease_holder`, and both are per-ticket. It is therefore safe to return it in a conflict error (`C-ERR-4`). |
| `C-FENCE-2` | A claim-protected write must put **both** `lease_holder = :actor` and `lease_generation = :generation` in the `WHERE`. Generation alone is not sufficient (`C-CLAIM-16`). |
| `C-FENCE-3` | `version` is optimistic concurrency, not ownership. A stale `expected_version` from the *current holder* is still rejected with `version_conflict`; the holder must re-read. |
| `C-FENCE-4` | A renewal changes neither number, so an agent can hold a lease across many heartbeats and its `version` stays valid for content edits until someone else actually changes the ticket. |

### 3.2 Which operations require which

The full per-operation matrix is [`docs/concurrency.md`](concurrency.md) — normative for REST and MCP
alike. **If this document and `concurrency.md` disagree, `concurrency.md` wins and this document is the
defect to fix** — that includes the §8.2 authorization cells below, not only the summary table
(`C-OPEN-10`). The summary:

| Operation class | `expected_version` | `generation` (fencing) | Notes |
|---|---|---|---|
| `claim`, `claim_next` | optional (if supplied, mismatch → `version_conflict`) | **no** | A claim is a transition, not a write against a known state; requiring the generation would make the first claim impossible (`generation = 0`). |
| `renew_claim`, `release_claim` | no | **required** | Both are holder-only; the generation is the proof of holding. |
| Claim-protected ticket mutation by an **agent**: ticket content/status/priority/assignee/resolution/epic, ticket delete, handoff create, memory-write submit (`docs/concurrency.md` §3–§4) | **required** | **required** | `C-FENCE-5`. Task, note and relation writes are **open** even on a claimed ticket (`C-OPEN-1`–`C-OPEN-3`). |
| Same content / status / delete mutation by an **orchestrator** | **required** | **required** | An orchestrator is an automated writer and is fenced exactly like an agent (`C-GUARD-4`, `C-AUTH-8`); `human` is the only unfenced actor type. The one exception is handoff create and memory-write submit, which `C-HANDOFF-13` lets an orchestrator do without a lease. |
| Same content / status / delete mutation by a **human** | optional | not required | Humans are not fenced (spec §5.5, `C-FENCE-6`); they still bump `version` and, for ownership-changing overrides, the generation. |
| `reassign`, `revoke`, `unblock` | no | not required | Override authority; `override: true` is required over a live lease. |
| Board / epic CRUD, ticket create, bulk create | no | no | Not ticket-owned; idempotency only (§4). |
| Handoff create / accept | **required** on create by an agent | **required** | A handoff is the strongest ownership claim there is; it must prove holding. |

| ID | Rule |
|---|---|
| `C-FENCE-5` | An `agent`'s or `orchestrator`'s content mutation without a `generation` is `400 invalid_request` (missing required field), **not** a 409: the caller made a malformed request, not a losing race. A *wrong* generation is `409 lease_generation_mismatch`. |
| `C-FENCE-6` | A human's content mutation without `expected_version` is allowed and is last-write-wins, but is audited with `previous_version` so the lost update is visible afterwards. `[LEAD-DECIDED]` — the UI has no version in hand for every field edit; forcing one would break the drag-and-drop and inline-edit flows that must keep working (`C-COMPAT-2`). |
| `C-FENCE-7` | Any accepted mutation that changes client-visible state increments `version` exactly once, regardless of how many fields changed. |

### 3.3 What invalidates a generation

| Trigger | Old generation | New generation | Old holder's token |
|---|---|---|---|
| claim | invalid | issued | dead |
| reassign (with or without override) | invalid | issued | dead |
| revoke | invalid | issued | dead |
| release by holder | invalid | issued (holder `NULL`) | dead |
| expiry sweep | invalid | issued (holder `NULL`) | dead |
| handoff accept | invalid | issued | dead |
| renew | **still valid** | unchanged | live |
| content edit, task/note/relation change | still valid | unchanged | live |
| status change **to** `done` or `blocked`, or **leaving** `blocked` | invalid, and the holder is cleared | issued | dead |
| status change between `todo` and `in_progress` with no lease held | unchanged | unchanged | n/a |

| ID | Rule |
|---|---|
| `C-FENCE-8` | The table above is exhaustive. If an operation is not in it, it does not touch `lease_generation`. |
| `C-FENCE-9` | A stale holder's claim-protected write after a release **must** fail: the release bumped the generation, so `lease_generation = :old` matches nothing even though `lease_holder` is now `NULL` (and `NULL = :actor` is never true). Verified by the probe (`stale release with dead generation -> 0 rows`). |
| `C-FENCE-10` | There is no "grace window" and no clock skew allowance: a token is valid iff `lease_expires_at > now_ms` and the two guards match, evaluated in one statement. |

### 3.4 Where the spec's §5.5 "all overrides invalidate prior claim authority" lands

| ID | Rule |
|---|---|
| `C-FENCE-11` | Overrides and ownership-bearing writes are classified by whether they change **ownership-bearing state**, and only those bump the generation: `reassign`, `revoke`, a holder's own `release` (`C-CLAIM-18`), status change to/from `done` or `blocked`, and `unblock`. A pure **content edit** by a human bumps `version` only. `[LEAD-DECIDED]` — read literally, §5.5 would kick an agent off its ticket because a human fixed a typo in the description, which would make the board unusable; the version bump already forces the agent to re-read before its next write, so no stale write survives. The classification is a single table in the service layer, so if the owner wants the literal reading it is a one-row change, not a redesign. **Flagged for the owner** — see `[OWNER-1]` in §10. |
| `C-FENCE-12` | An agent can never present itself as a human: the actor type comes from server configuration (`C-AUTH-1`), never from the request body. Any `actor`, `actor_type`, `generation` or `lease_*` field in a request body is ignored and reported as an unknown field (`C-COMPAT-5`). |

---

## 4. Idempotency rules

### 4.1 Scope, key and operation names

| ID | Rule |
|---|---|
| `C-IDEM-1` | The scope is the triple **`(actor, operation, key)`** — spec §7. Two different agents may use the same key string without colliding, and one agent may use the same key for two different operations. The uniqueness is the primary key of `idempotency_keys` (`C-SCHEMA-17`); a duplicate insert is the conflict signal. |
| `C-IDEM-2` | `key` is 1–128 characters from `[A-Za-z0-9._:-]`. Anything else is `400 invalid_request` with `details.field = "idempotency_key"`. It is **never** derived from the payload server-side — the client owns it. |
| `C-IDEM-3` | `operation` is one of the fixed names below. It is assigned by the service layer, not the client, so a client cannot make two different operations share a scope by lying. |

| `operation` | Transport entry points | Key required? |
|---|---|---|
| `ticket.create` | `POST /api/v1/boards/{id}/tickets`, MCP `ticket{create}` | **optional** |
| `ticket.bulk_create` | `POST /api/v1/boards/{id}/tickets/bulk`, MCP `ticket{bulk_create}` | **optional** |
| `ticket.claim` | `POST /api/v1/tickets/{id}/claim`, `POST /api/v1/boards/{id}/claim-next`, MCP `claim{claim\|next}` | **required** |
| `handoff.create` | `POST /api/v1/tickets/{id}/handoffs`, MCP `handoff{create}` | **required** |
| `handoff.accept` | `POST /api/v1/handoffs/{id}/accept`, MCP `handoff{accept}` | **required** |
| `memory.write.submit` | `POST /api/v1/tickets/{id}/memory-writes`, MCP `memory{write}` | **required** |
| `memory.write.deliver` | internal worker only (never a client entry point) | the outbox `id` **is** the key |

| ID | Rule |
|---|---|
| `C-IDEM-4` | Optional means: when the key is absent, the request executes normally with no idempotency record. Required means: absent → `400 invalid_request` with `details.field = "idempotency_key"`. `[LEAD-DECIDED]` — ticket create and bulk create exist in upstream clients that know nothing about keys (`C-COMPAT-2`); the claim/handoff/memory operations are new, so requiring the key there costs no compatibility and buys the guarantee exactly where a retry is dangerous. |
| `C-IDEM-5` | `memory.write.deliver` never accepts a client key: the stable outbox `id` (`C-SCHEMA-14`) is passed to the adapter as its idempotency key on **every** attempt, so a duplicate delivery is deduplicable at the far end. |

### 4.2 Replay semantics

| ID | Rule |
|---|---|
| `C-IDEM-6` | **Same key + same payload hash** → the original response is replayed: same HTTP status, same body **bytes** (`response_body` is stored verbatim), plus `Idempotency-Replayed: true` (REST header) / `"replayed": true` (MCP result field). The entity is **not** re-read: a replay returns what the first call returned, even if the ticket has changed since. |
| `C-IDEM-7` | **Same key + different payload hash** → `409 idempotency_conflict`, details `{"operation", "key", "request_hash"}` (the *stored* hash, not the new one). The original request is unaffected. |
| `C-IDEM-8` | Only **2xx** responses are recorded. A 4xx/5xx is never stored, so retrying a failed request with the same key re-executes it. `[LEAD-DECIDED]` — caching an error would freeze a transient failure into a permanent one for 72 h. |
| `C-IDEM-9` | A replay never re-publishes SSE and never writes a second audit row: the mutation did not happen again. |
| `C-IDEM-10` | `request_hash` is the lowercase hex SHA-256 of the **canonical JSON of the decoded request**: object keys sorted by byte value, no insignificant whitespace, UTF-8, arrays in order, numbers in shortest round-trip form, and fields the server ignores (unknown fields, `null` vs absent treated as distinct only when the operation distinguishes them) excluded. Hashing the raw body bytes is forbidden — field order and whitespace are not semantics. |
| `C-IDEM-11` | For `ticket.bulk_create`, the hash covers the whole array; a replay returns the same array of tickets with the same ids, not a second batch. |

### 4.3 Storage and crash behaviour

| ID | Rule |
|---|---|
| `C-IDEM-12` | The idempotency row is inserted in the **same transaction** as the mutation it records, and `response_body` is filled in before `COMMIT`. Therefore: either the mutation and its recorded response both exist, or neither does. There is no window in which a committed mutation lacks its key row. |
| `C-IDEM-13` | Consequently a crash can only produce: (a) no key row and no mutation — a retry executes cleanly; or (b) both — a retry replays. It can never produce a mutation whose response was lost while the key row says it succeeded, because both commit together. `[LEAD-DECIDED]` — this is why the design needs no two-phase "in flight" reconciliation. |
| `C-IDEM-14` | `status` has two values, `in_flight` and `completed`. `in_flight` exists only inside an uncommitted transaction and is therefore **never observable after commit**; if a reader ever sees it, the row is stale and the request is treated as retryable (`C-IDEM-8`). The column exists so the schema states the intent and a future multi-process deployment can keep the same shape. |
| `C-IDEM-15` | Two concurrent requests with the same key: the second blocks on the write lock (up to `busy_timeout`), then finds the completed row and replays it. It must **not** return `409 idempotency_in_progress` merely because the first is still running unless the wait exceeded `busy_timeout`, in which case the response is `503 database_busy` (retryable, `C-ERR-8`). |
| `C-IDEM-16` | Retention is **72 hours** (`expires_at = created_at + 259_200_000`). Purge runs (a) on startup and (b) opportunistically at most once every 10 minutes inside a write transaction, deleting `WHERE expires_at < :now_ms`. A purge never runs inside a request that is itself being deduplicated. The 10-minute rate limit lives in **process memory** — one `last_purge_ms` value on the store, no table and no column — so each server process keeps its own schedule and two processes on one database may both purge. That is safe and intended: a purge is one idempotent `DELETE` under the single writer (`C-CONV-3`), and **no correctness depends on it running at all** (`C-IDEM-19`), so the shared-state question is deliberately not answered with shared state. `[LEAD-DECIDED]` — the spec says "e.g. 72h"; a fixed 72 h is what the docs and tests assert. |
| `C-IDEM-17` | Keys are **server-side only**: never exposed as a URL path segment, never echoed in an error other than `C-IDEM-7`'s details, never written to the audit payload (only `request_id` is), and never used as an entity id. |
| `C-IDEM-18` | Purging a key never affects the entity it created. After expiry, the same key is simply a new request again — which is the documented consequence of a 72 h window and must be stated in the API docs. |
| `C-IDEM-19` | **An expired key row behaves as absent, purged or not.** `C-IDEM-16` is housekeeping, not the expiry mechanism: a request whose key matches a row with `expires_at <= :now_ms` executes as a new request (`C-IDEM-18`) even if no purge has run. The order is: attempt the `INSERT` of the new key row; if it fails on the primary key (`C-SCHEMA-17`), re-read that row **for diagnosis only**, and if it is expired, `DELETE` it and `INSERT` again inside the same transaction. Both writes are unconditional — the read never decides *whether* to write, only which error it is explaining (`C-CONV-4`). A duplicate against an unexpired row is the ordinary replay/conflict path (`C-IDEM-6`, `C-IDEM-7`). This is what makes the purge best-effort and its state in-process (`C-IDEM-16`). |

---

## 5. Handoff JSON schema v1

### 5.1 The document

```json
{
  "schema_version": 1,
  "id": "9f2c1b7a4e5d40c9a1b2c3d4e5f60718",
  "ticket_id": "83f681a8a8864a1cbe1deff69e6570e9",
  "board_id": "d3251bb6a7604239a8361c9c90ae3296",
  "from_actor": "agent:builder-1",
  "from_actor_type": "agent",
  "to_actor": "agent:builder-2",
  "generation": 7,
  "status": "offered",
  "supersedes": null,
  "summary": "Parser rewrite is done and green; next is wiring the CLI flag.",
  "done": ["Rewrote the tokenizer", "Added 14 table tests"],
  "next": ["Wire --strict flag", "Update README section 4"],
  "blockers": ["Needs the owner's decision on the wire value"],
  "artifacts": [
    {"kind": "commit", "ref": "a1b2c3d", "description": "tokenizer rewrite"},
    {"kind": "pr", "ref": "https://github.com/jatr3vin0/Kanban/pull/12", "description": "phase 2"}
  ],
  "memory_writes": [
    {"type": "finding", "content": "modernc.org/sqlite 3.51.2 supports UPDATE ... RETURNING", "confidence": 0.9, "tags": ["sqlite", "driver"]}
  ],
  "memory_refs": ["mem_01J9X8Z4QK"],
  "created_at": "2026-10-30T23:08:22.180Z",
  "accepted_by": null,
  "accepted_at": null
}
```

### 5.2 Fields

| Field | Type | Required | Limits / validation |
|---|---|---|---|
| `schema_version` | integer | yes | must be exactly `1`. Any other value → `400 handoff_invalid`, `details.field = "schema_version"`. No forward-compat guessing: a future version is a new contract. |
| `id` | string | server-assigned | 32 lowercase hex. A client-supplied `id` is ignored and reported as an unknown field. |
| `ticket_id` | string | yes (path) | must exist; must match the path/argument ticket id, else `400 handoff_invalid`. |
| `board_id` | string | server-assigned | derived from the ticket, never trusted from the body. |
| `from_actor` | string | server-assigned | the authenticated actor (`C-AUTH-1`); a body value is ignored (`C-FENCE-12`). |
| `from_actor_type` | string | server-assigned | one of `human`, `agent`, `orchestrator`, `system`. |
| `to_actor` | string \| null | no | ≤ 200 chars, `[A-Za-z0-9._:-]`. `null` = offered to whoever claims next. |
| `generation` | integer | server-assigned | the caller's fencing generation at creation (`C-SCHEMA-11`). |
| `status` | string | server-assigned | `offered` on creation. |
| `supersedes` | string \| null | no | an existing handoff id on the same ticket; self-reference → `400 handoff_invalid`. |
| `summary` | string | **yes** | 1–4000 chars after `TrimSpace`; must be non-empty. This is the field an agent reads first, so an empty summary is rejected rather than defaulted. |
| `done` | array\<string\> | no (default `[]`) | ≤ 50 items, each 1–500 chars after trim; empty strings dropped, not rejected. |
| `next` | array\<string\> | no (default `[]`) | same limits as `done`. |
| `blockers` | array\<string\> | no (default `[]`) | ≤ 20 items, each 1–500 chars. Free text, not ticket ids — a blocker that is a ticket is expressed as a `blocks` relation, which the board already understands. `[LEAD-DECIDED]` |
| `artifacts` | array\<object\> | no (default `[]`) | ≤ 50 items. Each `{kind, ref, description?}`: `kind` ∈ `commit`, `pr`, `file`, `url`, `log`, `other`; `ref` 1–1000 chars, no whitespace-only, **no `user:pass@` userinfo in URLs** (`400 handoff_invalid`); `description` ≤ 500 chars. |
| `memory_writes` | array\<object\> | no (default `[]`) | ≤ 50 items. Each `{type, content, confidence?, tags?}` — see §5.3. |
| `memory_refs` | array\<string\> | no (default `[]`) | ≤ 50 items, each 1–200 chars. Ids of records already in the operator's memory, used to link a handoff to prior knowledge. |
| `created_at`, `accepted_by`, `accepted_at` | string / null | server-assigned | never accepted from a request. |

| ID | Rule |
|---|---|
| `C-HANDOFF-1` | The document is validated at the **service boundary**, so REST, MCP and any future CLI get identical validation (recon G11). Validation never lives in a handler. |
| `C-HANDOFF-2` | Total size: the canonical JSON of the document must be ≤ **65 536 bytes**; over that → `400 handoff_invalid`, `details.field = "document"`. Per-field limits above apply first, so the total check is a backstop. |
| `C-HANDOFF-3` | `memory_writes[].content` is 1–2000 chars and is the only free-text field that will be sent to the operator's memory system, so it is validated as strictly as `summary`. |
| `C-HANDOFF-4` | Unknown fields are handled exactly like every other request: warn by default, reject under strict mode (`C-COMPAT-5`). A typo in `memory_writes` must never be silently dropped, because it means a durable memory write that never happens. |

### 5.3 Memory types and confidence

| ID | Rule |
|---|---|
| `C-HANDOFF-5` | `memory_writes[].type` ∈ **`fact`**, **`decision`**, **`requirement`**, **`finding`**, **`preference`**, **`continuation`**, **`lesson`** (spec §8). Anything else → `400 handoff_invalid`, `details.field = "memory_writes[3].type"`, `details.allowed = [...]`. Validated in Go, not by a `CHECK` (`C-CONV-5`). |
| `C-HANDOFF-6` | `memory_writes[].confidence` is a JSON number in **[0.0, 1.0]** inclusive, optional, default **1.0**. Outside the range, `NaN`, `Infinity` or a string → `400 handoff_invalid`. `[LEAD-DECIDED]` — default 1.0 for a write an agent chose to make deliberately; a lower value is a statement the agent makes on purpose. |
| `C-HANDOFF-7` | `memory_writes[].tags` is ≤ 16 items of ≤ 64 chars, `[A-Za-z0-9._:-]`; duplicates are collapsed preserving first-seen order. |
| `C-HANDOFF-8` | The same `(type, content)` pair may appear twice in one handoff; both are enqueued. Deduplication against the operator's memory is the adapter's business, not ours (`C-OUTBOX-12`). |

### 5.4 Canonical form and immutability

| ID | Rule |
|---|---|
| `C-HANDOFF-9` | **The checksummed field set is the creation-time fields, and it is exactly these seventeen:** `schema_version`, `id`, `ticket_id`, `board_id`, `from_actor`, `from_actor_type`, `to_actor`, `generation`, `supersedes`, `summary`, `done`, `next`, `blockers`, `artifacts`, `memory_writes`, `memory_refs`, `created_at`. `status`, `accepted_by` and `accepted_at` are **outside** the hash, because `accept` and supersession write them and a checksum that moved on a legitimate transition would report drift on every accept. The rule is stated by mutability rather than by authorship: every field fixed at creation is inside, every field the two permitted transitions write is outside, and `C-SCHEMA-9` is what protects the excluded three. The **canonical form** is that set rendered with object keys sorted by byte value, no insignificant whitespace, arrays in submitted order, `null`-valued optional fields omitted, and `created_at` with three fractional digits; its lowercase hex SHA-256 is `handoffs.checksum` (`C-SCHEMA-10`), recomputed from the typed columns and never from a stored copy of the document, so a client can detect drift in any creation-time field at any point in the handoff's life. |
| `C-HANDOFF-10` | **Handoffs are immutable under ordinary operations.** There is no update path, no field patch, and no delete. The only writes after creation are the acceptance transition (`status`, `accepted_by`, `accepted_at`) and supersession (`status` → `superseded`). |
| `C-HANDOFF-11` | A **correction is a new handoff** with `supersedes = <old id>`; the old row's `status` becomes `superseded` in the same transaction. The chain is walked newest-first and must not contain a cycle; a cycle attempt → `400 handoff_invalid`. |
| `C-HANDOFF-12` | The `supersedes` chain is scoped to one ticket: superseding a handoff that belongs to a different ticket → `400 handoff_invalid`. |

### 5.5 Creation, atomicity and ownership

| ID | Rule |
|---|---|
| `C-HANDOFF-13` | Creating a handoff is a **claim-protected** operation: an `agent` must present the ticket's current `lease_holder` + `lease_generation` (`C-FENCE-2`); a `human`/`orchestrator` may create one without a lease (audited with `from_actor_type = human`). |
| `C-HANDOFF-14` | Creating a handoff and inserting all of its `outbox` rows happen in **one transaction** (spec §8/§10). There is no state in which a handoff exists without its memory-write obligations, and none in which obligations exist for a handoff that was rejected. |
| `C-HANDOFF-15` | Creating a handoff does **not** change ownership by itself: the author keeps the lease (and its generation) until it releases or another actor accepts. This lets an agent write two handoffs in a row without losing the ticket. `[LEAD-DECIDED]` |
| `C-HANDOFF-16` | `accept` is the ownership transfer: in one statement it sets `lease_holder = acceptor`, `lease_holder_type = acceptor type`, `lease_acquired_at = now`, `lease_expires_at = now + ttl`, `lease_generation = lease_generation + 1`, `status = 'in_progress'`, `version = version + 1`, `handoff_id = <id>`, and marks the handoff `accepted` with `accepted_by`/`accepted_at`. The guard is `status = 'offered' AND (to_actor IS NULL OR to_actor = :acceptor)`; 0 rows → `409 handoff_not_acceptable`. |
| `C-HANDOFF-17` | Accepting a handoff **does not require holding the lease first**: an offered handoff addressed to a specific actor is the authority to take over. Accepting one addressed to someone else is `409 handoff_not_acceptable`. |
| `C-HANDOFF-18` | `tickets.handoff_id` is always the most recently **accepted** handoff. A superseded handoff does not change it. |

### 5.6 Retrieval shapes

All four shapes exist on both transports, with identical field names.

| Shape | REST | MCP | Returns |
|---|---|---|---|
| latest | `GET /api/v1/tickets/{id}/handoff` | `handoff{action:"latest", ticket_id}` | the newest handoff by `created_at` (any status), full document. `404 not_found` when the ticket has none. |
| history | `GET /api/v1/tickets/{id}/handoffs` | `handoff{action:"history", ticket_id}` | array of full documents, newest first; empty array (not 404) when none. |
| by id | `GET /api/v1/handoffs/{id}` | `handoff{action:"get", id}` | one full document. `404 not_found` when absent. |
| compact | `GET /api/v1/tickets/{id}/handoff?compact=true` | `handoff{action:"latest", ticket_id, compact:true}` | the shape below. |

Compact shape (designed for an agent's context budget — no lists, only counts):

```json
{
  "id": "9f2c1b7a4e5d40c9a1b2c3d4e5f60718",
  "ticket_id": "83f681a8a8864a1cbe1deff69e6570e9",
  "schema_version": 1,
  "from_actor": "agent:builder-1",
  "from_actor_type": "agent",
  "to_actor": "agent:builder-2",
  "status": "offered",
  "generation": 7,
  "created_at": "2026-10-30T23:08:22.180Z",
  "summary": "Parser rewrite is done and green; next is wiring the CLI flag.",
  "done_count": 2, "next_count": 2, "blocker_count": 1,
  "artifact_count": 2, "memory_write_count": 1, "memory_ref_count": 1
}
```

| ID | Rule |
|---|---|
| `C-HANDOFF-19` | `summary` is truncated in the compact shape only at 500 chars, with `summary_truncated: true` appended. Every other compact field is a count, never a list. |
| `C-HANDOFF-20` | History is paginated by `?limit=` (default 20, max 100) and `?before=<created_at ms>`; the response carries `next_before` when more exist. Ordering is `created_at DESC, id DESC` so equal-millisecond handoffs are still totally ordered. |
| `C-HANDOFF-21` | A handoff for a ticket the caller cannot see is `404 not_found`, never `403`: existence is not leaked. (v1 has no per-ticket visibility, so this is currently unobservable — it is the rule the deferred scopes in §8.4 must preserve.) |

---

## 6. Outbox states and the adapter contract

### 6.1 State machine

```
                 enqueue (in the handoff transaction)
                              │
                              ▼
                         ┌─────────┐
      retryable ────────► │ pending │ ◄──── (MEMORY_DELIVERY=off: stays here)
         ▲               └────┬────┘
         │                    │ claim (lease 60 s, attempts+1)
         │                    ▼
         │             ┌─────────────┐   adapter returned an id + local ack committed
         │             │ processing  │ ─────────────────────────────────► ┌───────────┐
         │             └──────┬──────┘                                   │ succeeded │
         │                    │                                          └───────────┘
         │      ┌─────────────┼──────────────────┐
         │      │             │                  │
         │  definite      ambiguous         lease expired
         │  failure       failure           (worker died)
         │      │             │                  │
         │      ▼             ▼                  ▼
         │ ┌───────────┐ ┌───────────┐   ┌───────────┐
         └─┤ retryable │ │ uncertain │──►│ uncertain │
            └─────┬─────┘ └─────┬─────┘   └───────────┘
                  │             │
        attempts exhausted or permanent failure
                  │             │
                  ▼             ▼
              ┌────────┐    ┌────────┐
              │ failed │    │ failed │
              └────────┘    └────────┘
```

| ID | Rule |
|---|---|
| `C-OUTBOX-1` | The status set is exactly **`pending`, `processing`, `retryable`, `succeeded`, `uncertain`, `failed`**, validated in Go (`C-CONV-5`). |
| `C-OUTBOX-2` | `succeeded` is the only state that asserts the operator's memory has the write. It is set only after the adapter returned a non-empty external id **and** the local `UPDATE ... WHERE id = ? AND lease_owner = ?` committed with `RowsAffected() == 1`. |
| `C-OUTBOX-3` | `uncertain` means: an attempt was made and we cannot know whether the far end accepted it. It is **never** reported as success and never silently retried as if it had failed. It is retried (safely — `C-OUTBOX-9`), but the state is visible in the UI and in `GET /api/v1/outbox?status=uncertain` until it resolves. |
| `C-OUTBOX-4` | `failed` is terminal (dead-letter): attempts exhausted, or the adapter rejected the write as permanently invalid. It is recoverable only by an explicit human retry (`C-OUTBOX-13`). |

### 6.2 Fields

| Field | Type | Meaning |
|---|---|---|
| `id` | TEXT PK | stable write id (`C-SCHEMA-14`); the adapter idempotency key |
| `ticket_id`, `handoff_id`, `board_id` | TEXT | origin; no FK (`C-SCHEMA-13`) |
| `actor`, `actor_type` | TEXT | who asserted this memory, from server identity (`C-AUTH-1`) |
| `memory_type` | TEXT | the §5.3 enum |
| `content` | TEXT | 1–2000 chars |
| `confidence` | REAL | 0.0–1.0 |
| `tags_json` | TEXT | JSON array |
| `status` | TEXT | §6.1 |
| `attempts` | INTEGER | incremented **at claim time**, not at completion, so a crash mid-attempt still counts |
| `next_attempt_at` | INTEGER ms \| NULL | earliest next claim; `NULL` = immediately claimable |
| `lease_owner` | TEXT \| NULL | worker id holding the processing lease |
| `lease_expires_at` | INTEGER ms \| NULL | processing lease expiry (60 s) |
| `external_id` | TEXT \| NULL | id returned by the adapter on success |
| `last_error` | TEXT \| NULL | ≤ 500 chars, scrubbed (`C-SCHEMA-16`) |
| `created_at`, `updated_at` | INTEGER ms | server time |
| `delivered_at` | INTEGER ms \| NULL | set with `succeeded` |
| `request_id` | TEXT | correlation id from the originating request |
| `idempotency_key` | TEXT | equal to `id` on creation; kept as a column so the adapter call never has to assume it |

### 6.3 Claiming work and recovering stuck workers

```sql
-- one statement; a worker claims at most one row per call
UPDATE outbox
   SET status = 'processing',
       lease_owner = :worker_id,
       lease_expires_at = :now_ms + 60000,
       attempts = attempts + 1,
       updated_at = :now_ms
 WHERE id = (
   SELECT id FROM outbox
    WHERE ( (status IN ('pending','retryable','uncertain')
             AND (next_attempt_at IS NULL OR next_attempt_at <= :now_ms))
         OR (status = 'processing' AND lease_expires_at <= :now_ms) )
    ORDER BY created_at ASC, id ASC
    LIMIT 1)
RETURNING id, ticket_id, handoff_id, actor, actor_type, memory_type,
          content, confidence, tags_json, attempts, idempotency_key;
```

| ID | Rule |
|---|---|
| `C-OUTBOX-5` | A `processing` row whose lease expired is **reclaimable by any worker** — that is the stuck-worker recovery, and it needs no separate reaper process. |
| `C-OUTBOX-6` | Reclaiming an expired `processing` row first sets it to **`uncertain`** (in the same transaction as the claim, or in a one-statement pre-pass `UPDATE ... SET status='uncertain' WHERE status='processing' AND lease_expires_at <= :now_ms`), because the previous worker may have delivered before dying. `[LEAD-DECIDED]` — `attempts` is already ≥ 1 by definition, so "unknown" is the only honest label. |
| `C-OUTBOX-7` | A worker whose ack affects 0 rows (its lease was stolen or expired) must **not** report success to anyone and must not write `external_id`. The row belongs to whoever holds the lease now. |

### 6.4 Delivery semantics

| ID | Rule |
|---|---|
| `C-OUTBOX-8` | Delivery is **at-least-once**. A write may reach the operator's memory more than once. Correctness therefore rests on the stable `id` being passed as the adapter idempotency key (`C-IDEM-5`) and on the adapter deduplicating by it. The README and `/docs` must say this in one sentence. |
| `C-OUTBOX-9` | **Success-then-crash-before-local-ack.** If the adapter accepts the write and the process dies before the `succeeded` update commits, the row is still `processing`. Its lease expires, `C-OUTBOX-6` marks it `uncertain`, and a later worker retries with the same `id`. The far end sees a duplicate it can deduplicate; the board never claims a success it cannot prove. This is the case the spec calls out, and it is why `C-OUTBOX-2` requires the committed local ack, not just the adapter's return value. |
| `C-OUTBOX-10` | Retry schedule (seconds after the failed attempt): `30, 120, 600, 1800, 7200, 21600, 21600` — `attempts` 1…7 — and **`attempts >= 8` → `failed`**. `next_attempt_at = now_ms + backoff`. A permanent rejection (`adapter_rejected`) goes straight to `failed` regardless of attempts. |
| `C-OUTBOX-11` | Error classification is explicit, never by string matching (recon §10: driver and adapter errors carry codes, and message text is not stable): the adapter returns a `*DeliveryError{Code, Retryable, Err}`. `Retryable = true` → `retryable`; `Retryable = false` → `failed`; **any error the adapter does not classify, and any timeout or context cancellation after the request was sent, is `uncertain`**. |
| `C-OUTBOX-12` | No deduplication against the operator's memory is attempted on our side. We do not search before writing: that would be a read-then-write across an external system, and it cannot be made atomic with the write. `[LEAD-DECIDED]` |
| `C-OUTBOX-13` | `POST /api/v1/outbox/{id}/retry` (human/orchestrator only) resets a `failed` row to `pending` with `attempts = 0`, `next_attempt_at = NULL`, `last_error = NULL`, and audits it. Resetting a `succeeded` row → `409 outbox_not_retryable`. |

### 6.5 `MEMORY_DELIVERY=off|worker`

| ID | Rule |
|---|---|
| `C-OUTBOX-14` | `MEMORY_DELIVERY` accepts exactly `off` and `worker`; the default is **`off`** (spec §10). Any other value is a startup error, not a silent fallback. |
| `C-OUTBOX-15` | With `off`: no worker goroutine starts, **and outbox rows are still created and visible**. Nothing is delivered, nothing is marked `failed`, nothing is dropped. `GET /api/v1/outbox` returns the backlog. |
| `C-OUTBOX-16` | Switching from `off` to `worker` later delivers the whole backlog in `created_at` order — that is the intended path once the operator's memory server is reachable. No migration or replay command is needed. |
| `C-OUTBOX-17` | With `worker`: exactly one goroutine loop per process claims one row at a time (the single writer connection serialises anyway), sleeping `LEDGERBOARD_WORKER_POLL_MS` (`C-CONV-10`, default `1000` ms) when nothing is claimable. The sleep is a `select` on the worker's shutdown context, so shutdown never waits out a poll interval. A second process on the same database is safe (`C-OUTBOX-5`) but wasteful; the docs say so rather than pretending it is unsupported. |
| `C-OUTBOX-18` | The worker must never start when the adapter is not configured: `MEMORY_DELIVERY=worker` with no adapter is a **startup error**, because a worker that immediately fails every row would turn a configuration mistake into a pile of dead letters. |

### 6.6 The adapter interface

```go
// MemoryWrite is one durable assertion the board wants the operator's memory to hold.
type MemoryWrite struct {
    ID         string    // stable write id == outbox.id; the adapter idempotency key
    TicketID   string
    HandoffID  string
    Actor      string    // "agent:builder-1"
    ActorType  string
    Type       string    // fact|decision|requirement|finding|preference|continuation|lesson
    Content    string
    Confidence float64
    Tags       []string
    CreatedAt  time.Time // UTC
}

type MemoryRecord struct {
    ExternalID string
    Type       string
    Content    string
    Confidence float64
    Tags       []string
    CreatedAt  time.Time
}

// Adapter is the only thing the outbox worker knows about the outside world.
type Adapter interface {
    // Write must be idempotent on w.ID: a repeated Write with the same ID must
    // return the same ExternalID, not create a second record.
    Write(ctx context.Context, w MemoryWrite) (externalID string, err error)
    Get(ctx context.Context, externalID string) (MemoryRecord, error)
}

// Searcher is OPTIONAL. Implementations that cannot search simply do not implement it.
type Searcher interface {
    Search(ctx context.Context, query string, limit int) ([]MemoryRecord, error)
}

type DeliveryError struct {
    Code      string // adapter_unavailable | adapter_timeout | adapter_rejected | adapter_not_configured
    Retryable bool
    Err       error
}
```

| ID | Rule |
|---|---|
| `C-OUTBOX-19` | `Write` and `Get` are required; `Search` is optional and detected by a type assertion to `Searcher` at wiring time, once, not per call. |
| `C-OUTBOX-20` | **Documented fallback when `Search` is missing:** the search operation returns `501 search_unsupported` with `details.reason = "adapter_has_no_search"` and `details.fallback = "use memory_refs in handoffs"`. It must **never** return an empty list, because an empty list is indistinguishable from "no results" and would make an agent believe knowledge does not exist. The agent-facing documentation says: when search is unsupported, use `memory_refs` from the latest handoff and re-derive. |
| `C-OUTBOX-21` | The mock adapter (Phase 5) implements all three and records calls in memory; the real operator-memory adapter is blocked until the Python MCP server is reachable (see `recon.md` §9/§12 and the business plan's "Blocked externally"). The outbox contract does not change when it arrives. |
| `C-OUTBOX-22` | The adapter never sees the SQLite database, the ticket row, or anything not in `MemoryWrite` — so a future out-of-process adapter (an MCP client to the operator's memory server) is a drop-in. |

### 6.7 Observability

| ID | Rule |
|---|---|
| `C-OUTBOX-23` | `GET /api/v1/outbox?status=&ticket_id=&limit=&offset=` returns outbox rows newest-first, with counts per status in a sibling endpoint `GET /api/v1/outbox/summary`. Both are `human`/`orchestrator` readable; an `agent` may read only rows for tickets it currently holds. `[LEAD-DECIDED]` — the backlog is operational state, not ticket content. |
| `C-OUTBOX-24` | Every delivery transition writes exactly one audit row on the ticket (`memory_delivered` / `memory_delivery_failed`, §1.8). Retries within `retryable` do not write one per attempt — the attempt counter is the record; only terminal transitions are audited, so a flapping adapter cannot flood the timeline. |

---

## 7. Error codes

### 7.1 One enum, two representations

| ID | Rule |
|---|---|
| `C-ERR-1` | There is exactly **one** error type in the service layer: `Error{Code Code, Message string, Details map[string]any, RequestID string}`. Both transports render it from one mapping table. A handler never invents an error code and never maps a status itself. |
| `C-ERR-2` | `Code` is a closed enum (§7.3). Adding a code is a contract change; a code string that is not in the table is a defect, and the renderer maps it to `internal` while logging loudly. |
| `C-ERR-3` | REST renders: `Content-Type: application/json; charset=utf-8`, body = the JSON object in §7.2. `[LEAD-DECIDED]` — the body has always been JSON; it was served as `text/plain` only because the code used `http.Error`. Clients that parse the body are unaffected; clients that never parse it are unaffected. |
| `C-ERR-4` | Ownership-conflict details **must** carry, when known: `ticket_id`, `status`, `version`, `holder`, `holder_type`, `lease_expires_at`, `lease_generation`, `reason`. `holder` is an actor id (`agent:builder-1`), never a token. `lease_generation` is included: it is not a secret and is useless without the matching holder (`C-FENCE-1`). |
| `C-ERR-5` | No error ever contains a credential, a token, an adapter URL with userinfo, or a raw driver error string. An unexpected internal error renders as `{"error_code":"internal","error":"internal server error"}` with the real error logged server-side and the `request_id` in both places so the log and the response can be joined. |
| `C-ERR-6` | An unknown internal error is `500 internal`, never a leak of `err.Error()`. This is already upstream behaviour (recon §4.1) and is preserved. |
| `C-ERR-7` | `database_busy` is `503` with `Retry-After: 1` and is the only code that is safe to retry blindly. Recon §4.5 measured `SQLITE_BUSY` surfacing as a silent 500; after the DSN fix (`C-CONV-9`) it should be unreachable, so if it appears it is a real signal, not noise. |

### 7.2 REST body

```json
{
  "error": "ticket 83f6… is held by agent:builder-1 until 2026-10-30T23:23:22.180Z",
  "error_code": "already_claimed",
  "details": {
    "ticket_id": "83f681a8a8864a1cbe1deff69e6570e9",
    "status": "in_progress",
    "version": 12,
    "holder": "agent:builder-1",
    "holder_type": "agent",
    "lease_expires_at": "2026-10-30T23:23:22.180Z",
    "lease_generation": 4,
    "reason": "lease_active"
  },
  "request_id": "req_01J9X8Z4QK"
}
```

| ID | Rule |
|---|---|
| `C-ERR-8` | `error` is preserved verbatim in shape — a human sentence, same key, same meaning. Old clients keep working (`C-COMPAT-1`). `error_code`, `details` and `request_id` are additive; `details` is omitted when empty, `request_id` is always present (generated when the caller sent none). |
| `C-ERR-9` | Warnings (unknown fields, deprecated fields) are **not** errors: they are `200` responses with a `warnings: ["unknown field \"lease_expires_at\" ignored"]` array (`C-COMPAT-5`). |

### 7.3 MCP representation

```json
{
  "jsonrpc": "2.0", "id": 3,
  "result": {
    "content": [
      {"type": "text", "text": "already_claimed: ticket 83f6… is held by agent:builder-1 until 2026-10-30T23:23:22.180Z"},
      {"type": "text", "text": "{\"error_code\":\"already_claimed\",\"error\":\"ticket 83f6… is held by …\",\"details\":{…},\"request_id\":\"req_01J9X8Z4QK\"}"}
    ],
    "structuredContent": {"error": {"code": "already_claimed", "message": "…", "details": {…}, "request_id": "req_01J9X8Z4QK"}},
    "isError": true
  }
}
```

| ID | Rule |
|---|---|
| `C-ERR-10` | Business failures stay **tool-level** results with `isError: true` — never JSON-RPC protocol errors. That is upstream behaviour (recon §3) and clients depend on it. |
| `C-ERR-11` | The first text block is always `<code>: <message>`, so a client that only reads prose gets a stable, machine-matchable prefix (today it gets prose only — recon §3). The second text block is the full JSON object, so the code and details are readable even if the SDK drops `structuredContent` on error results. |
| `C-ERR-12` | `structuredContent.error` is the same object as the second text block. If `mcp-go` refuses `structuredContent` on an `isError` result, Phase 3 records that in the PR and the second text block remains the contract. The two text blocks are mandatory either way, so no client is left without a machine-readable code. |
| `C-ERR-13` | Success results are unchanged: bare object for object results, `{"items": [...]}` for lists (recon §3). Only the error path gains structure. |

### 7.4 The code table (normative)

| `error_code` | REST | When | Required `details` |
|---|---|---|---|
| `invalid_request` | 400 | malformed JSON, missing required field, bad enum, bad key format | `field` |
| `unknown_field` | 400 | unknown JSON field **in strict mode** (`C-COMPAT-5`) | `fields[]` |
| `not_found` | 404 | entity absent; also `claim_next` with nothing claimable | `ticket_id` / `board_id` / `handoff_id` / `reason` |
| `forbidden` | 403 | actor type may not perform the operation (`C-AUTH-4`) | `actor_type`, `operation` |
| `unauthorized` | 401 | **reserved** for the deferred remote-auth phase; never emitted in v1 | — |
| `already_claimed` | 409 | a live lease is held by another actor | `C-ERR-4` set + `override_required:true` on `reassign` |
| `not_claimed` | 409 | `revoke` with nothing to revoke | `ticket_id` |
| `not_claim_holder` | 409 | caller is not the current holder | `ticket_id`, `holder`, `holder_type` |
| `lease_expired` | 409 | `renew` by the holder after the lease lapsed (`C-CLAIM-14`); **never** `release`, which has no expiry guard (`C-CLAIM-18`) | `ticket_id`, `lease_expires_at`, `lease_generation` |
| `lease_generation_mismatch` | 409 | fencing token stale | `ticket_id`, `expected_generation`, `current_generation` |
| `version_conflict` | 409 | `expected_version` stale | `ticket_id`, `expected_version`, `current_version` |
| `status_not_claimable` | 409 | claim on `done`/`blocked`/non-claimable status | `ticket_id`, `status`, `claimable_statuses[]` |
| `dependency_blocked` | 409 | unfinished `blocks` dependency | `ticket_id`, `blocking_ticket_ids[]`, `blocking_statuses[]` |
| `idempotency_conflict` | 409 | same key, different payload | `operation`, `key`, `request_hash` |
| `idempotency_in_progress` | 409 | **reserved**; only for a future multi-process deployment (`C-IDEM-15`) | `operation`, `key` |
| `handoff_invalid` | 400 | schema violation, oversize document, bad enum/confidence | `field`, `reason` |
| `handoff_not_acceptable` | 409 | handoff not `offered`, or addressed to someone else | `handoff_id`, `status`, `to_actor` |
| `outbox_not_retryable` | 409 | retry of a `succeeded` row | `outbox_id`, `status` |
| `memory_delivery_disabled` | 409 | delivery requested while `MEMORY_DELIVERY=off` | `memory_delivery` |
| `search_unsupported` | 501 | adapter has no `Search` (`C-OUTBOX-20`) | `reason`, `fallback` |
| `payload_too_large` | 413 | **not used in v1** — reserved; an oversize handoff is `400 handoff_invalid` so one field has one failure mode (`C-HANDOFF-2`) | `max_bytes` |
| `rate_limited` | 429 | **reserved** for the deferred remote phase | `retry_after_ms` |
| `database_busy` | 503 | write lock not acquired within `busy_timeout` | `retry_after_ms` |
| `internal` | 500 | anything unexpected | — |

| ID | Rule |
|---|---|
| `C-ERR-14` | `409` is the status for **every** ownership conflict (spec §5.6), and the code distinguishes the cause. A client that only knows "409 means someone else has it" still behaves correctly; a client that reads `error_code` can do better. |
| `C-ERR-15` | A 404 is never used to signal a conflict and a 409 is never used to signal absence. The diagnosis order in `C-CLAIM-22` is what guarantees this. |

---

## 8. Authorization matrix

### 8.1 Actor types and where identity comes from

| ID | Rule |
|---|---|
| `C-AUTH-1` | There are exactly four actor types: **`human`**, **`agent`**, **`orchestrator`**, **`system`**. Identity is always server-derived — from trusted local configuration or a session, never from a request body (`C-FENCE-12`). |
| `C-AUTH-2` | Identity has two parts: `actor_type` (above) and `actor_id` (a string matching `[A-Za-z0-9._:-]{1,200}`, e.g. `builder-1`). Together they render as `type:id` in `lease_holder`, `ticket_events.actor` and `handoffs.from_actor`. |
| `C-AUTH-3` | v1 sources, in precedence order: (1) **MCP stdio** — the process configuration (`LEDGERBOARD_ACTOR_TYPE`, `LEDGERBOARD_ACTOR`), because one stdio process is one agent; (2) **MCP HTTP** — the `X-Ledgerboard-Actor-Type` / `X-Ledgerboard-Actor` headers presented at `initialize`, fixed for the session; (3) **REST** — the same headers per request; (4) fallback — the process defaults `agent:local` / type `agent`. |
| `C-AUTH-4` | Trusting those headers is gated: `LEDGERBOARD_TRUST_ACTOR_HEADER` defaults to **on when the listener is bound to a loopback address** and **off otherwise**. With it off, the headers are ignored and the process default is used. `--trust-actor-header` on a non-loopback bind requires `--allow-remote-unauthenticated`, which prints a startup warning. `[LEAD-DECIDED]` — spec §13 allows "trusted local config"; a loopback-only header is exactly that, and the spec's §19 localhost default makes it safe by construction. |
| `C-AUTH-5` | **v1 is a trusted-local boundary, not a security boundary.** Any local process that can open a socket to a loopback-bound server can present any actor type. This is stated plainly here and in the README; it is not a bug to be discovered later. Recon D9 is the reason: upstream has no identity at all, so every rule below is an honour system that becomes enforceable when the deferred scopes arrive. |
| `C-AUTH-6` | `system` is never accepted from a client header. It is only used by in-process code (the expiry sweeper, the outbox worker, migrations). A client presenting `system` → `403 forbidden`. |

### 8.2 What each actor may do

| Operation | `human` | `agent` | `orchestrator` | `system` |
|---|---|---|---|---|
| Read anything (boards, tickets, handoffs, audit, outbox summary) | yes | yes | yes | yes |
| Create / edit / delete boards, epics | yes | no | yes | no |
| Create tickets (single, bulk) | yes | yes | yes | no |
| **Claim a ticket** | **no** (`403`) | yes | yes | internal only |
| Renew / release own claim | yes, when it holds one (T8, handoff accept) | yes | yes | no |
| Edit ticket content / status / delete | yes, unfenced | yes, **fenced** (`C-FENCE-2`, `C-GUARD-4`) | yes, **fenced** (`C-FENCE-2`, `C-GUARD-4`) | no |
| Edit tasks / notes / relations (ticket children) | yes, **open** | yes, **open** (`C-OPEN-1`–`C-OPEN-4`) | yes, **open** | no |
| Create a handoff | yes | yes, fenced | yes | no |
| Accept a handoff | yes | yes (if addressed or `to_actor` null) | yes | no |
| **Reassign** a ticket (override a live claim) | yes | **no** (`403`) | yes | no |
| **Revoke** a claim | yes | **no** (`403`) | yes | no |
| **Unblock** a ticket | yes | **no** (`403`) | yes | no |
| Read the outbox | yes | only rows for tickets it holds | yes | yes |
| Retry a dead-lettered memory write | yes | no | yes | no |
| Change `MEMORY_DELIVERY` | no (config only) | no | no | no |

| ID | Rule |
|---|---|
| `C-AUTH-7` | The `agent` column has **five** "no" cells, and each is a `403 forbidden` — a *capability* failure — never a 409: create/edit/delete boards or epics (structure is the operator's, `C-AUTH-9`); **reassign** (an agent cannot give itself a ticket it did not win); **revoke** (it cannot clear a claim that is not its own); **unblock** (it cannot undo a block a human set, `C-CLAIM-2`); and retry a dead-lettered memory write (dead-letter recovery is an operator decision, `C-OUTBOX-13`). The table's other agent-restricted line, changing `MEMORY_DELIVERY`, is "no" for all four actor types — it is configuration rather than a capability cell, so it is not counted here. Count them from §8.2, not from this sentence. |
| `C-AUTH-8` | `orchestrator` is a human-equivalent for **overrides** (it is the human's fleet controller) and may **hold a lease like an agent**, but on **ticket content, status and delete writes it is fenced exactly like an agent, whether or not it holds the lease**: `expected_version` is always required (`C-GUARD-4`), and where a live lease exists the claim guard applies (`C-GUARD-1`–`C-GUARD-3`), so a non-holder orchestrator gets `409 already_claimed` and a holding one must also present its `generation` (`C-FENCE-2`). `human` is the only unfenced actor type in §8.2 (`C-FENCE-6`). The one deliberate carve-out is handoff create and memory-write submit, where `C-HANDOFF-13` lets an orchestrator act without a lease — those are how a fleet *reports and proposes knowledge*, not how it edits work in flight. `[LEAD-DECIDED]` — spec §13 lists orchestrator alongside agent for claim-like behaviour and alongside human for override behaviour; the human side of that is a **capability** distinction (what it may do), never a **fencing** exemption (what it must prove). An orchestrator is an automated writer that edits at fleet scale and always has the ticket it read in hand, so giving it the human's last-write-wins path would reopen the exact lost-update hole `C-GUARD-4` closes. |
| `C-AUTH-9` | An `agent` may create tickets (that is how a fleet files follow-up work) but may not create boards or epics (structure stays human). `[LEAD-DECIDED]` — the alternative (agents create boards) has no user, and a board is the operator's organisational unit. |

### 8.3 Human override versus agent action

| ID | Rule |
|---|---|
| `C-AUTH-10` | The distinction is exactly `actor_type` on the request identity plus the capability table. A `human` or `orchestrator` override on a claimed ticket is audited as `claim_reassigned` — with `override:true` when a live lease existed (`C-GUARD-7`) — or as `claim_revoked` (`C-GUARD-8`), and `actor` always names the actor that performed it (`human:<id>` / `orchestrator:<id>`); a holder's own release, by any actor type that can hold a lease (T3), is `claim_released` with `reason:"release"` and no `override` field. There is no non-holder `claim_released` path: taking work away from another actor is `reassign` (T5) or `revoke` (T6). The audit trail therefore answers "who" for the first time (recon §5.1: `actor` was always `''`). |
| `C-AUTH-11` | "Agents cannot impersonate humans" (spec §5.5) is enforced by `C-AUTH-1`/`C-AUTH-6`: there is no request field that sets the actor type, and `system` is rejected outright. Under v1's trusted-local model this prevents mistakes and sloppy clients, not a determined local attacker (`C-AUTH-5`). |
| `C-AUTH-12` | The existing `assignee` field is **not** an identity: it stays free text, writable by anyone, and never grants or revokes authority (`C-SCHEMA-5`). The UI may display the claimant next to the assignee label, but the two are never conflated. |

### 8.4 Where the deferred scopes slot in

The spec defers remote auth scopes to a later phase. This is the mapping they must use, so that nothing
has to be re-cut later:

| Scope | Operations |
|---|---|
| `read` | every `GET`, MCP `list`/`get`/`history`/`context`/`ready`/`summary`/`latest` |
| `write` | ticket/epic/board/task/note/relation mutations, handoff create, memory-write submit |
| `claim` | `claim`, `claim_next`, `renew`, `release`, handoff accept |
| `admin` | `reassign`, `revoke`, `unblock`, outbox retry, outbox inspection, config |

| ID | Rule |
|---|---|
| `C-AUTH-13` | The capability table in §8.2 is expressed in terms of these four scopes internally from day one (`human` ⇒ read+write+claim+admin, `agent` ⇒ read+write+claim, `orchestrator` ⇒ all four, `system` ⇒ all four but not over a transport), so the deferred work is "populate the scope from a credential" and not "rewrite the authorization". |
| `C-AUTH-14` | When remote auth lands, `unauthorized` (401) and `rate_limited` (429) become reachable and no other code changes. They are reserved now so that the enum is stable. |

---

## 9. Compatibility rules

Each rule is a testable promise. "Old client" means any client written against upstream `c6938a2`
(recon §3, §4); "old database" means a database migrated only to `0001`.

| ID | Promise | Test |
|---|---|---|
| `C-COMPAT-1` | **No envelope.** Success bodies stay bare objects and bare arrays. `GET /api/v1/boards` returns `[...]`, `POST /api/v1/boards` returns the board object, not `{ok, data}`. New fields may be added; existing keys keep their names and types. | Create a board with an old-style body; assert the response has `id`/`name`/`description`/`created_at`/`updated_at` at the top level and no `ok`/`data` keys. |
| `C-COMPAT-2` | **`in_progress` stays the wire value.** `{"status":"in_progress"}` → 200; `{"status":"in progress"}` → 400 with `status must be one of: todo, in_progress, done, blocked` (unchanged message). The UI continues to display "In Progress". | Both requests, assert 200 and 400 respectively; grep the frontend column labels. |
| `C-COMPAT-3` | **Unknown fields warn by default.** A body with an unknown field is accepted, the field is ignored, and the response carries `warnings: ["unknown field \"lease_expires_at\" ignored"]`. Under strict mode (`LEDGERBOARD_STRICT_JSON=1` or `X-Ledgerboard-Strict: 1`) the same request is `400 unknown_field` with `details.fields`. Recon §4.2 shows today's silent drop; the warning is the additive fix. | `PUT /tickets/{id}` with `{"lease_expires_at": …, "title": "x"}` → 200 + warning (default) and 400 (strict). |
| `C-COMPAT-4` | **Old databases keep working, in place.** A database at `0001` migrates on open to `0002` with no data change beyond column defaults; a second open is a no-op; `foreign_key_check` is clean afterwards. | Copy a `0001` database, open, assert `schema_migrations` version 2, `version=1`/`lease_generation=0` on every pre-existing ticket, `foreign_key_check` empty, second open clean. |
| `C-COMPAT-5` | **Old binaries keep working against a new database.** `0002` only adds nullable-or-defaulted columns and new tables, so an upstream binary's `INSERT`/`UPDATE` (which name their columns) still succeeds and its reads ignore the new columns. | Run the upstream binary against a `0002` database; create and update a ticket; assert 2xx. (This is the strongest test that the migration is genuinely additive.) |
| `C-COMPAT-6` | **Timestamps stop changing shape.** `created_at`/`updated_at` are serialized from the stored value, so the create response and a subsequent `GET` return the **same string**. Today they disagree — create echoes nanoseconds, reads return seconds (recon R6). `[LEAD-DECIDED]` — this is a bug fix, and no client can depend on the same object having two formats. | Create a ticket, capture `created_at`, `GET` it, assert byte equality. |
| `C-COMPAT-7` | **SSE keeps its shape.** Event name stays `board_change`, payload keeps `type` and `board_id`. New keys (`ticket_id`, `actor`, `version`) may be added; existing clients that read only `board_id` (the bundled UI does exactly that, recon §4.8) are unaffected. | Subscribe, mutate, assert `event: board_change` and that `board_id` is present and correct. |
| `C-COMPAT-8` | **SSE is published from the service layer**, so a mutation made over MCP now emits an event (recon §5.2: today MCP emits nothing). This is additive in the sense that clients receive *more* events for the same board; the UI's cache-invalidation behaviour is unaffected. Publishing happens after commit, never before. | Perform the same mutation over REST and over MCP; assert both produce one `board_change` for the board. |
| `C-COMPAT-9` | **The six existing MCP tools keep their names, actions and argument names** (recon §3). New capabilities arrive as new tools (`claim`, `handoff`, `memory`) and new optional arguments; no existing action is renamed or removed. `tools/list` annotations are corrected to truthful `readOnlyHint`/`destructiveHint` values, which is strictly more permissive than the current all-destructive advertisement. | `tools/list` contains the six original tools with unchanged `inputSchema` required fields; a scripted call to each original action still works. |
| `C-COMPAT-10` | **Error bodies keep `error`.** Every error response still has a top-level `error` string with the same meaning as before; `error_code`, `details`, `request_id` are additive (`C-ERR-8`). | Assert `error` present and a string on 400/404/409/500. |
| `C-COMPAT-11` | **Default bind is `127.0.0.1`** (spec §19), with `--host`/`KANBAN_HOST` to opt back into `0.0.0.0`. This is a deliberate behaviour change from upstream's `:port` (recon §4.9), required by the spec and documented in the README's breaking-changes section. | Start with no flags, assert the listener is on loopback only; start with `--host 0.0.0.0`, assert all interfaces. |
| `C-COMPAT-12` | **Legacy rows with values the Go enums do not know are readable but not writable.** A ticket whose `status` is anything other than the four known values is returned as-is (the UI renders the raw string) and any attempt to write an unknown status is rejected. Recon G17: the database has never enforced enums and existing rows must not break the server. | Insert a ticket with `status='weird'` directly, `GET` → 200, `PUT {"status":"weird"}` → 400. |

| ID | Rule |
|---|---|
| `C-COMPAT-13` | These twelve promises are the *whole* compatibility contract. Anything not listed here is free to change; anything listed here needs a contract revision and a note in the README's breaking-changes section to change. |

---

## 10. Decisions the owner must make

Three items are genuinely the owner's. Nothing else in this document waits on them.

### `[OWNER-1]` — does a human content edit revoke an agent's claim?

Spec §5.5 lists "content edit" among the human overrides that "invalidate prior claim authority". Read
literally, a human fixing a typo in a claimed ticket's description would end the agent's ownership.

- **Option A (contracted, `C-FENCE-11`):** a human content edit bumps `version` only. The agent's lease
  survives; its next write fails with `version_conflict` and it re-reads. Ownership-bearing overrides
  (reassign, release, revoke, status to/from done or blocked, unblock) do bump the generation.
- **Option B (literal §5.5):** any human edit of a claimed ticket ends the claim, issuing a new generation.

Option A is implemented unless the owner says otherwise; switching to B is a one-row change in the
override classification table, not a redesign. The cost of B is that the live UI becomes hostile to
agents: every human keystroke on a claimed ticket kicks the agent off mid-task.

### `[OWNER-2]` — handoff retention

Handoffs are immutable and are never deleted (`C-HANDOFF-10`). A long-lived board therefore grows without
bound: each handoff is up to 64 KiB.

- **Option A:** keep everything forever. Simplest, best audit trail, unbounded growth (a busy board with
  10 handoffs/day is ~230 MB/year at the cap, far less in practice).
- **Option B:** prune superseded handoffs older than N days, keeping the latest per ticket. Bounds growth;
  destroys history; needs an export first to avoid silent data loss.
- **Option C:** no pruning in v1, with a documented `VACUUM`/export procedure in the operator docs.

The contract assumes **A/C** (no automatic deletion). Pruning is a data-loss decision, so it is not taken
here.

### `[OWNER-3]` — upstream license (inherited from recon §9, not new)

Upstream `rgracey/kanban-mcp` has **no license file** — all rights reserved. Engineering can continue on
the fork, but a modified redistribution or a shipped binary needs upstream's permission. This does not
block Phase 2 or Phase 3; it blocks publishing. Re-stated here because this contract is the first
document that describes the product as shippable.

---

## 11. What this contract deliberately does not decide

- **Remote auth, TLS and scopes** — deferred; §8.4 is the slot they fill (`C-AUTH-13`).
- **Multi-process claim tests, crash boundaries, migration torture** — `docs/test-plan.md` (deliverable 2
  of Phase 1) turns each rule here into a test, including the five crash boundaries.
- **UI presentation** — Phase 4. The contract fixes the data (claimant, expiry, generation, outbox
  status, attribution) and nothing about how it looks.
- **The operator's memory integration** — blocked externally; the adapter interface (`C-OUTBOX-19`) and
  the mock are the whole v1 surface.
- **Rate limiting, pagination beyond handoffs, search** — not in v1; no rule here forbids them later.
