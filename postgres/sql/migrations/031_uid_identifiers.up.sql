-- uid: the ONLY externally-visible identifier (api-contract §3; Aaron's
-- 2026-07-03 ruling: internal BIGSERIAL ids appear in NO public contract,
-- including the library-mode Go API). uid is generated Go-side as UUIDv7 on
-- every insert — deliberately no DEFAULT so a write path that forgets to
-- supply one fails loudly instead of minting a second id source.
--
-- Rows that exist before this migration (library consumers upgrading a live
-- database, plus the two classifications seeded by migration 011) get a
-- one-time gen_random_uuid() backfill — v4, not v7, which is fine: uniqueness
-- is the only property the contract needs; v7's time-ordering is a Go-side
-- nicety for new rows. Hence the add-backfill-then-NOT-NULL shape per table
-- rather than a bare ADD COLUMN ... NOT NULL (which fails on any non-empty
-- table).
--
-- Deliberately plain per-table DDL (not a DO $$ ... EXECUTE loop): sqlc's
-- static parser derives the schema from these files and cannot see columns
-- created via dynamic SQL. Keep every statement statically parseable.

ALTER TABLE journals ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE journals SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE journals ALTER COLUMN uid SET NOT NULL;

ALTER TABLE bookings ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE bookings SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE bookings ALTER COLUMN uid SET NOT NULL;

ALTER TABLE events ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE events SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE events ALTER COLUMN uid SET NOT NULL;

ALTER TABLE reservations ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE reservations SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE reservations ALTER COLUMN uid SET NOT NULL;

ALTER TABLE classifications ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE classifications SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE classifications ALTER COLUMN uid SET NOT NULL;

ALTER TABLE journal_types ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE journal_types SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE journal_types ALTER COLUMN uid SET NOT NULL;

ALTER TABLE entry_templates ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE entry_templates SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE entry_templates ALTER COLUMN uid SET NOT NULL;

ALTER TABLE currencies ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE currencies SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE currencies ALTER COLUMN uid SET NOT NULL;

ALTER TABLE account_policies ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE account_policies SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE account_policies ALTER COLUMN uid SET NOT NULL;

ALTER TABLE period_closes ADD COLUMN IF NOT EXISTS uid UUID;
UPDATE period_closes SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE period_closes ALTER COLUMN uid SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_journals_uid         ON journals (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_uid         ON bookings (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_events_uid           ON events (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_reservations_uid     ON reservations (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_classifications_uid  ON classifications (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_journal_types_uid    ON journal_types (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_entry_templates_uid  ON entry_templates (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_currencies_uid       ON currencies (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_account_policies_uid ON account_policies (uid);
CREATE UNIQUE INDEX IF NOT EXISTS uq_period_closes_uid    ON period_closes (uid);
