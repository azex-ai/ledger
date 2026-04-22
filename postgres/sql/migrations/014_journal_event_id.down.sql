DROP INDEX IF EXISTS idx_journals_event;
ALTER TABLE journals DROP COLUMN IF EXISTS event_id;
