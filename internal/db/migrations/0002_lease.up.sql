-- 0002_lease: the ownership engine's additive schema (contracts.md §1.2–§1.6).
--
-- Every statement here is an ALTER TABLE ... ADD COLUMN, a CREATE INDEX or a
-- CREATE TABLE (C-SCHEMA-1). No DROP, no RENAME, no table rebuild, no data
-- UPDATE, no trigger: `tickets` has four cascading FK children, so a rebuild
-- would have to re-create them, and an upstream binary must still be able to
-- read and write this database afterwards (C-COMPAT-5).
--
-- Every added column is nullable or has a constant default, so this is legal on
-- a populated table and existing rows get their contracted values without a
-- rewrite: version = 1, lease_generation = 0, lease_renewals = 0, every lease
-- column NULL, handoff_id NULL (C-SCHEMA-2).

ALTER TABLE tickets ADD COLUMN version           INTEGER NOT NULL DEFAULT 1;
ALTER TABLE tickets ADD COLUMN lease_holder      TEXT;
ALTER TABLE tickets ADD COLUMN lease_holder_type TEXT;
ALTER TABLE tickets ADD COLUMN lease_acquired_at INTEGER;
ALTER TABLE tickets ADD COLUMN lease_expires_at  INTEGER;
ALTER TABLE tickets ADD COLUMN lease_generation  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tickets ADD COLUMN lease_renewals    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tickets ADD COLUMN handoff_id        TEXT;

CREATE INDEX tickets_claimable    ON tickets(board_id, status, lease_expires_at);
CREATE INDEX tickets_lease_holder ON tickets(lease_holder) WHERE lease_holder IS NOT NULL;

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
