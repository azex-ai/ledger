-- uid: the ONLY externally-visible identifier (api-contract §3; Aaron's
-- 2026-07-03 ruling: internal BIGSERIAL ids appear in NO public contract,
-- including the library-mode Go API). uid is generated Go-side as UUIDv7 on
-- every insert — deliberately NOT NULL with no DEFAULT so a write path that
-- forgets to supply one fails loudly instead of minting a second id source.
-- No-legacy premise: these tables are empty when this migration runs, with
-- one exception — migration 011 seeds two classification rows, which get a
-- uid backfilled here (the only backfill in the whole scheme).
ALTER TABLE journals         ADD COLUMN uid UUID NOT NULL;
ALTER TABLE bookings         ADD COLUMN uid UUID NOT NULL;
ALTER TABLE events           ADD COLUMN uid UUID NOT NULL;
ALTER TABLE reservations     ADD COLUMN uid UUID NOT NULL;
ALTER TABLE journal_types    ADD COLUMN uid UUID NOT NULL;
ALTER TABLE entry_templates  ADD COLUMN uid UUID NOT NULL;
ALTER TABLE currencies       ADD COLUMN uid UUID NOT NULL;
ALTER TABLE account_policies ADD COLUMN uid UUID NOT NULL;
ALTER TABLE period_closes    ADD COLUMN uid UUID NOT NULL;

-- classifications carries the 011-seeded 'deposit' / 'withdraw' rows.
ALTER TABLE classifications ADD COLUMN uid UUID;
UPDATE classifications SET uid = gen_random_uuid() WHERE uid IS NULL;
ALTER TABLE classifications ALTER COLUMN uid SET NOT NULL;

CREATE UNIQUE INDEX uq_journals_uid         ON journals (uid);
CREATE UNIQUE INDEX uq_bookings_uid         ON bookings (uid);
CREATE UNIQUE INDEX uq_events_uid           ON events (uid);
CREATE UNIQUE INDEX uq_reservations_uid     ON reservations (uid);
CREATE UNIQUE INDEX uq_classifications_uid  ON classifications (uid);
CREATE UNIQUE INDEX uq_journal_types_uid    ON journal_types (uid);
CREATE UNIQUE INDEX uq_entry_templates_uid  ON entry_templates (uid);
CREATE UNIQUE INDEX uq_currencies_uid       ON currencies (uid);
CREATE UNIQUE INDEX uq_account_policies_uid ON account_policies (uid);
CREATE UNIQUE INDEX uq_period_closes_uid    ON period_closes (uid);
