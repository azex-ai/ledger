ALTER TABLE journals ALTER COLUMN actor_id DROP NOT NULL;
ALTER TABLE journals ALTER COLUMN source DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id DROP NOT NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at DROP NOT NULL;
