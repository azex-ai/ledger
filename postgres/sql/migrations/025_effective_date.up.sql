-- Effective date: separate the business date a journal is attributed to
-- (effective_at) from the write-time date it was posted (created_at). This
-- enables retroactive posting (late invoices, delayed on-chain confirmations)
-- and is the foundation for period close (migration 026) and trial-balance
-- reporting. See docs/plans/2026-07-02-financial-core-hardening-design.md §1.
--
-- No external users yet (see design doc §0), so this is a single-step
-- destructive migration: existing rows are force-backfilled from created_at
-- rather than the usual expand/migrate/contract three-step dance.
ALTER TABLE journals
    ADD COLUMN IF NOT EXISTS effective_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Denormalized onto entries so as-of aggregation (trial balance, balance
-- trends, snapshots) does not need to join journals.
ALTER TABLE journal_entries
    ADD COLUMN IF NOT EXISTS effective_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE journals SET effective_at = created_at;

-- The 018 append-only row trigger blocks any UPDATE on journal_entries; this
-- one-time backfill is schema evolution, not a business mutation, so disable
-- it for the statement (recursively covers the partition clones) and restore
-- immediately. Without this the migration fails on any database that already
-- has journal rows (0-row fresh databases never fire the row trigger, which
-- is why CI alone did not catch it).
ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_update;
UPDATE journal_entries je SET effective_at = j.effective_at
    FROM journals j WHERE je.journal_id = j.id;
ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_no_update;

-- Drives as-of / trial-balance queries (WHERE currency_id = $1 AND
-- effective_at <= $2). Created on the partitioned parent; propagates to every
-- existing and future partition, same as idx_entries_account_id in 004_ledger.
CREATE INDEX IF NOT EXISTS idx_entries_currency_effective
    ON journal_entries (currency_id, effective_at);
