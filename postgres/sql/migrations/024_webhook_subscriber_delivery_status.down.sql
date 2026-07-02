ALTER TABLE webhook_subscribers
    DROP COLUMN IF EXISTS last_status_code,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS last_attempt_at;
