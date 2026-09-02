-- name: RecordDepositReorg :one
-- Upsert on (booking_uid, kind): the first detection opens the row, every
-- later detection only bumps last_seen_at, so "still observable on chain"
-- and "first noticed at" stay separate facts. Deliberately does NOT clear
-- resolved_at -- reopening a closed-out anomaly is an operator decision, not
-- something a recheck tick may do behind their back.
INSERT INTO deposit_reorgs (uid, kind, booking_uid, chain_id, tx_hash, journal_uid)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (booking_uid, kind) DO UPDATE SET
    last_seen_at = now()
RETURNING *;

-- name: GetOpenDepositReorg :one
SELECT * FROM deposit_reorgs
WHERE booking_uid = $1 AND kind = $2 AND resolved_at <= 'epoch';

-- name: ListOpenDepositReorgs :many
-- Oldest first: the on-call queue is worked front to back (RUNBOOK §12).
SELECT * FROM deposit_reorgs
WHERE resolved_at <= 'epoch'
ORDER BY id ASC
LIMIT $1;

-- name: ResolveDepositReorg :execrows
-- Closing out is idempotent by being conditional on the row still being
-- open: a second resolve affects zero rows instead of overwriting the first
-- operator's timestamp and note.
UPDATE deposit_reorgs
SET resolved_at = now(), resolution = $3
WHERE booking_uid = $1 AND kind = $2 AND resolved_at <= 'epoch';
