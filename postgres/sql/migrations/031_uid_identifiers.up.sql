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
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'journals', 'bookings', 'events', 'reservations', 'classifications',
        'journal_types', 'entry_templates', 'currencies', 'account_policies',
        'period_closes'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS uid UUID', t);
        EXECUTE format('UPDATE %I SET uid = gen_random_uuid() WHERE uid IS NULL', t);
        EXECUTE format('ALTER TABLE %I ALTER COLUMN uid SET NOT NULL', t);
    END LOOP;
END $$;

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
