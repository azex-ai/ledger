-- journals: nullable columns → NOT NULL with defaults
ALTER TABLE journals ALTER COLUMN actor_id SET DEFAULT 0;
UPDATE journals SET actor_id = 0 WHERE actor_id IS NULL;
ALTER TABLE journals ALTER COLUMN actor_id SET NOT NULL;

ALTER TABLE journals ALTER COLUMN source SET DEFAULT '';
UPDATE journals SET source = '' WHERE source IS NULL;
ALTER TABLE journals ALTER COLUMN source SET NOT NULL;

-- reservations: settled_amount nullable → NOT NULL DEFAULT 0
UPDATE reservations SET settled_amount = 0 WHERE settled_amount IS NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount SET DEFAULT 0;

-- reservations: journal_id nullable → NOT NULL DEFAULT 0
UPDATE reservations SET journal_id = 0 WHERE journal_id IS NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET DEFAULT 0;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_journal_id_fkey;

-- balance_checkpoints: last_entry_at nullable → NOT NULL DEFAULT epoch
UPDATE balance_checkpoints SET last_entry_at = 'epoch' WHERE last_entry_at IS NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at SET NOT NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at SET DEFAULT 'epoch';
