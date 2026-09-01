-- Index balance_checkpoints.updated_at so the unauthenticated /system/health
-- probe's GetCheckpointMaxAgeSeconds (MIN(updated_at)) is an index fetch, not
-- a sequential scan (2026-08-29 security review M-5). The probe is exempt from
-- auth and is typically the one path an ingress exposes, so a caller looping
-- on it forced a full scan of a table that grows one row per
-- (holder, currency, classification) dimension — a cheap way to exhaust the
-- connection pool and take the whole write path down with it. With this index
-- MIN(updated_at) resolves from the index's leftmost entry regardless of table
-- size.
--
-- Plain CREATE INDEX (not CONCURRENTLY): golang-migrate runs each migration in
-- a transaction, and CONCURRENTLY cannot run inside one; this matches how
-- 001_baseline builds every other index. On a very large existing deployment,
-- build this index out-of-band with CONCURRENTLY before applying the release
-- if the brief lock is a concern.
CREATE INDEX IF NOT EXISTS idx_balance_checkpoints_updated_at
    ON balance_checkpoints (updated_at);
