ALTER TABLE journals ADD COLUMN event_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_journals_event ON journals (event_id) WHERE event_id != 0;
