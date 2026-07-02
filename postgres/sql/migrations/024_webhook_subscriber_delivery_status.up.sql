-- Record the outcome of the most recent delivery attempt per subscriber so
-- operators can see which endpoints are failing without grepping application
-- logs (see docs/RUNBOOK.md §5 "Webhook delivery backlog"). No NULL policy:
-- NOT NULL with meaningful defaults, matching every other column on this table.
ALTER TABLE webhook_subscribers
    ADD COLUMN IF NOT EXISTS last_status_code INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch';
