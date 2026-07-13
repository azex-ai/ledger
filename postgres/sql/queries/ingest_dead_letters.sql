-- name: InsertIngestDeadLetter :one
-- Idempotent on idempotency_key: a repeated conflict on the same sighting
-- (e.g. the watcher retrying it every scan) is not a new alert -- ON CONFLICT
-- DO NOTHING means callers may get zero rows back (see IngestDeadLetterStore,
-- which treats pgx.ErrNoRows here as "already recorded", not an error).
INSERT INTO ingest_dead_letters (uid, chain_id, tx_hash, txlog_seq, idempotency_key, reason, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- name: ListIngestDeadLetters :many
-- Newest first, for on-call triage (RUNBOOK). Unbounded scope is not
-- expected -- ErrConflict should be rare; limit guards against a runaway
-- normalization bug flooding an operator's terminal.
SELECT * FROM ingest_dead_letters ORDER BY id DESC LIMIT $1;
