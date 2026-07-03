-- Revert to the 017 sentinel shape (NOT NULL DEFAULT 0, no FK).
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_journal_id_fkey;
UPDATE reservations SET journal_id = 0 WHERE journal_id IS NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET DEFAULT 0;
