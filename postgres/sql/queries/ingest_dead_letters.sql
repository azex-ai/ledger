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
-- Keyset pagination, NEWEST FIRST, for on-call triage (RUNBOOK §18) --
-- same shape and same reasoning as ListJournalsCursor: cursor_id = 0 means
-- "first page", the caller encodes the last (oldest) row's id as the opaque
-- next_cursor, and the next page is strictly older. A runaway normalization
-- bug can produce many of these, so the queue has to be walkable rather
-- than truncated at a "recent N" that hides the rest.
--
-- `booked` answers the only question that decides whether a row still needs
-- an operator: a dead letter whose deposit was booked afterwards -- replayed
-- by hand, or self-healed because the cause was a frozen account or a closed
-- period that has since reopened -- is history, not a queue item. The join is
-- on the shared deposit-{chain}-{tx}-{seq} idempotency key, which is by
-- construction the same string on both rows (bookings has a UNIQUE index on
-- it, so this is an index lookup, not a scan).
SELECT dl.*,
       (b.id IS NOT NULL)::BOOLEAN AS booked
FROM ingest_dead_letters dl
LEFT JOIN bookings b ON b.idempotency_key = dl.idempotency_key
WHERE (sqlc.arg(cursor_id)::bigint = 0 OR dl.id < sqlc.arg(cursor_id)::bigint)
ORDER BY dl.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetIngestDeadLetter :one
-- One dead letter, by uid, including the payload -- the serialized
-- core.DepositSighting a replay re-drives through IngestDeposit. Everything
-- a replay needs was already on the row before the replay path existed;
-- nothing read this column at all.
SELECT dl.*,
       (b.id IS NOT NULL)::BOOLEAN AS booked
FROM ingest_dead_letters dl
LEFT JOIN bookings b ON b.idempotency_key = dl.idempotency_key
WHERE dl.uid = $1;

-- name: CountUnbookedIngestDeadLetters :one
-- The backlog gauge's sample (core.Metrics.DeadLetterBacklog): how many dead
-- letters still have no booking, and when the oldest of those was recorded.
-- COALESCE keeps the empty-queue answer a timestamp rather than a NULL the
-- adapter would have to special-case -- 'epoch' is this schema's no-NULL
-- convention for "absent", and the caller reports a zero age for it.
SELECT count(*)::BIGINT AS unbooked,
       COALESCE(min(dl.created_at), 'epoch'::timestamptz)::TIMESTAMPTZ AS oldest_created_at
FROM ingest_dead_letters dl
WHERE NOT EXISTS (
    SELECT 1 FROM bookings b WHERE b.idempotency_key = dl.idempotency_key
);
