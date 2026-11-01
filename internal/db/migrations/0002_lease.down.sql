-- 0002_lease down: reverses 0002_lease.up.sql exactly (C-SCHEMA-4).
--
-- Destructive by definition: lease, handoff and outbox rows are lost, and
-- `tickets` returns to its eleven original columns with version/lease state
-- dropped. golang-migrate runs this only on an explicit Down request; nothing in
-- the product invokes it automatically.
--
-- Order is the reverse of the up migration: drop indexes before the columns
-- they cover (SQLite refuses to drop a column an index still references), then
-- the columns in reverse declaration order, then the tables.

DROP INDEX idempotency_expires;
DROP TABLE idempotency_keys;

DROP INDEX outbox_lease;
DROP INDEX outbox_handoff;
DROP INDEX outbox_ticket;
DROP INDEX outbox_claimable;
DROP TABLE outbox;

DROP INDEX handoffs_status;
DROP INDEX handoffs_ticket_created;
DROP TABLE handoffs;

DROP INDEX tickets_lease_holder;
DROP INDEX tickets_claimable;

ALTER TABLE tickets DROP COLUMN handoff_id;
ALTER TABLE tickets DROP COLUMN lease_renewals;
ALTER TABLE tickets DROP COLUMN lease_generation;
ALTER TABLE tickets DROP COLUMN lease_expires_at;
ALTER TABLE tickets DROP COLUMN lease_acquired_at;
ALTER TABLE tickets DROP COLUMN lease_holder_type;
ALTER TABLE tickets DROP COLUMN lease_holder;
ALTER TABLE tickets DROP COLUMN version;
