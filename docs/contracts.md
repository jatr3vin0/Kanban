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

---

## 1. Additive schema changes — `0002_lease.{up,down}.sql`

### 1.1 The promise

| ID | Rule |
|---|---|
| `C-SCHEMA-1` | `0002_lease.up.sql` contains **only** `ALTER TABLE ... ADD COLUMN` against `tickets`, `CREATE INDEX`, and `CREATE TABLE`. No `DROP`, no `RENAME`, no table rebuild, no data `UPDATE`, no trigger (recon G9: `tickets` has four cascading FK children — `ticket_events`, `notes`, `tasks`, `ticket_relations` — and a rebuild would have to re-create them). |
| `C-SCHEMA-2` | Every added column is nullable **or** has a constant default, so the statement is legal on a populated table and existing rows get a defined value without a rewrite. Existing rows: `version = 1`, `lease_generation = 0`, `lease_renewals = 0`, every lease column `NULL`, `handoff_id NULL`. |
| `C-SCHEMA-3` | The migration is applied by the existing `golang-migrate` path (`internal/db/db.go`, `schema_migrations` table). No `PRAGMA user_version` logic is introduced (recon: `user_version` is 0 and unused). |
| `C-SCHEMA-4` | `0002_lease.down.sql` reverses the migration exactly: drop indexes, then columns in reverse order, then tables. It is **destructive by definition** (lease, handoff and outbox rows are lost) and is never invoked automatically — `golang-migrate` runs `Down` only on explicit request. Round-trip verified against a populated database: `tickets` returns to its 11 original columns, `foreign_key_check` is clean. `[LEAD-DECIDED]` — upstream ships down migrations, so the convention is kept; the destructiveness is documented rather than hidden. |

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
| `C-SCHEMA-10` | The typed columns are the storage form; the v1 JSON document is rebuilt from them on read (§5). `checksum` is the lowercase hex SHA-256 of the canonical JSON of the v1 document as defined in §5.4, so any drift is detectable and idempotent replays compare cheaply. |
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
| `claim_released` | holder release, human release, or expiry sweep | `{"request_id","generation","previous_holder","actor_type","status_from","status_to","reason":"release"\|"expiry"\|"override"}` |
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
| T3 | `ACTIVE`/`LAPSED` → `UNCLAIMED` | `release_claim` | holder; human/orchestrator for anyone | holder + generation match (holder path) | `+1` | `+1` | 200 + ticket |
| T4 | `ACTIVE` → `LAPSED` → `UNCLAIMED` | expiry sweep | system | `lease_expires_at <= now` | `+1` | `+1` | audit only |
| T5 | any → `ACTIVE` | `reassign` | human, orchestrator | `override` required over a live lease (`C-CLAIM-15`) | `+1` | `+1` | 200 + ticket |
| T6 | `ACTIVE`/`LAPSED` → `UNCLAIMED` | `revoke` | human, orchestrator | ticket exists | `+1` | `+1` | 200 + ticket |
| T7 | `ACTIVE` → `ACTIVE` | `handoff` accept | current holder → `to_actor`, or claimer of an offered handoff | holder + generation match, or offered handoff | `+1` | `+1` | 200 + handoff |
| T8 | `ACTIVE` → `ACTIVE` | human takeover (reassign to self) | human | same as T5 | `+1` | `+1` | 200 + ticket |

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

| ID | Rule |
|---|---|
| `C-CLAIM-14` | **Renewal never resurrects a lapsed lease.** `lease_expires_at > :now_ms` is part of the `WHERE`. A lapsed holder gets `409 lease_expired` and must re-claim (competing with everyone else). `[LEAD-DECIDED]` — allowing an expired holder to renew would let a zombie process reclaim ownership after the ticket was legitimately swept, and would make `status` disagree with the lease. Guidance for slow agents: renew at 50 % of the TTL (7.5 min), not at expiry. |
| `C-CLAIM-15` | Renewal does **not** change `version` and does **not** change `lease_generation`. It is a heartbeat, not a state change: bumping `version` on every renewal would make an in-flight editor's `expected_version` fail for no reason. `[LEAD-DECIDED]` |
| `C-CLAIM-16` | `renew` requires `lease_holder` **and** `lease_generation` to match — the generation alone is never sufficient, because generations are per-ticket and would otherwise let actor B reuse actor A's number on the same ticket. |
| `C-CLAIM-17` | Release and revoke bump the generation so that a stale claim-protected write from the old holder cannot land on the now-unclaimed ticket (`§3.4`). |
| `C-CLAIM-18` | `release` with no live claim by that actor returns `409 not_claim_holder` (or `409 lease_expired` if the caller *was* the holder and the lease has lapsed — the diagnosis table in §2.7 decides). |

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
| `C-CLAIM-20` | The sweep is performed by the `system` actor and runs on a 60-second ticker inside the server process (a goroutine), plus once on startup. There is no external cron and no second binary. |
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
| | holder, generation, live lease, but status ≠ `in_progress` | `status_not_claimable` | 409 | `ticket_id`, `status` |
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

| ID | Rule |
|---|---|
| `C-CLAIM-22` | The diagnosis order in the table is normative: the **first** matching row is the error returned. "Ticket absent" is always first (404 before 409). |
| `C-CLAIM-23` | If the diagnosis SELECT finds nothing to explain the 0 rows, that is a `500 internal` and a logged defect — it means the guard and the diagnosis disagree, which must never happen silently. |
| `C-CLAIM-24` | A 0-row result is never retried automatically inside the store. Retry policy belongs to the caller (the agent), and only for `database_busy`. |

### 2.8 Invariants

| ID | Invariant | How it is enforced |
|---|---|---|
| `C-INV-1` | At most one active holder per ticket. | Ownership is a column on one row; there is no second place to record a holder. |
| `C-INV-2` | `lease_generation` is strictly monotonic per ticket: it only ever increases, by exactly 1, on an ownership transition; it is never reset and never decremented. | Every transition statement contains `lease_generation = lease_generation + 1`; no statement writes a literal generation. |
| `C-INV-3` | A ticket with `lease_holder IS NULL` has `lease_expires_at`, `lease_acquired_at` and `lease_holder_type` all `NULL` and `lease_renewals = 0`. | The release/sweep/revoke statements set all five together in one row update; a partial clear is a contract violation. |
| `C-INV-4` | A live lease (`lease_expires_at > now`) implies `status = 'in_progress'` unless a human changed the status; the status never silently reverts. | Claim sets `in_progress`; release/sweep only downgrade `in_progress`. |
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
alike. The summary:

| Operation class | `expected_version` | `generation` (fencing) | Notes |
|---|---|---|---|
| `claim`, `claim_next` | optional (if supplied, mismatch → `version_conflict`) | **no** | A claim is a transition, not a write against a known state; requiring the generation would make the first claim impossible (`generation = 0`). |
| `renew_claim`, `release_claim` | no | **required** | Both are holder-only; the generation is the proof of holding. |
| Claim-protected content mutation by an **agent** (ticket update, task/note/relation writes, status change) | **required** | **required** | `C-FENCE-5`. |
| Same mutation by a **human** or **orchestrator** | optional | not required | Humans are not fenced (spec §5.5); they still bump `version` and, for ownership-changing overrides, the generation. |
| `reassign`, `revoke`, `unblock` | no | not required | Override authority; `override: true` is required over a live lease. |
| Board / epic CRUD, ticket create, bulk create | no | no | Not ticket-owned; idempotency only (§4). |
| Handoff create / accept | **required** on create by an agent | **required** | A handoff is the strongest ownership claim there is; it must prove holding. |

| ID | Rule |
|---|---|
| `C-FENCE-5` | An agent's content mutation without a `generation` is `400 invalid_request` (missing required field), **not** a 409: the caller made a malformed request, not a losing race. A *wrong* generation is `409 lease_generation_mismatch`. |
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
| content edit, task/note/relation change, status change | still valid | unchanged | live |

| ID | Rule |
|---|---|
| `C-FENCE-8` | The table above is exhaustive. If an operation is not in it, it does not touch `lease_generation`. |
| `C-FENCE-9` | A stale holder's claim-protected write after a release **must** fail: the release bumped the generation, so `lease_generation = :old` matches nothing even though `lease_holder` is now `NULL` (and `NULL = :actor` is never true). Verified by the probe (`stale release with dead generation -> 0 rows`). |
| `C-FENCE-10` | There is no "grace window" and no clock skew allowance: a token is valid iff `lease_expires_at > now_ms` and the two guards match, evaluated in one statement. |

### 3.4 Where the spec's §5.5 "all overrides invalidate prior claim authority" lands

| ID | Rule |
|---|---|
| `C-FENCE-11` | Human/orchestrator overrides are classified by whether they change **ownership-bearing state**, and only those bump the generation: `reassign`, `release`, `revoke`, status change to/from `done` or `blocked`, and `unblock`. A pure **content edit** by a human bumps `version` only. `[LEAD-DECIDED]` — read literally, §5.5 would kick an agent off its ticket because a human fixed a typo in the description, which would make the board unusable; the version bump already forces the agent to re-read before its next write, so no stale write survives. The classification is a single table in the service layer, so if the owner wants the literal reading it is a one-row change, not a redesign. **Flagged for the owner** — see `[OWNER-1]` in §10. |
| `C-FENCE-12` | An agent can never present itself as a human: the actor type comes from server configuration (`C-AUTH-1`), never from the request body. Any `actor`, `actor_type`, `generation` or `lease_*` field in a request body is ignored and reported as an unknown field (`C-COMPAT-5`). |
