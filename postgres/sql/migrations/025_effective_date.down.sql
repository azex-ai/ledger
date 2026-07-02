DROP INDEX IF EXISTS idx_entries_currency_effective;
ALTER TABLE journal_entries DROP COLUMN IF EXISTS effective_at;
ALTER TABLE journals DROP COLUMN IF EXISTS effective_at;
