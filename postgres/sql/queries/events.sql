-- name: InsertEvent :one
INSERT INTO events (
    classification_code, booking_id, account_holder, currency_id,
    from_status, to_status, amount, settled_amount, journal_id,
    metadata, occurred_at, actor_id, source, uid
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: LinkEventJournal :one
UPDATE events
SET journal_id = $2
WHERE id = $1
  AND journal_id IS NULL
RETURNING *;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: GetLatestEventForBooking :one
SELECT * FROM events
WHERE booking_id = $1
ORDER BY id DESC
LIMIT 1;

-- name: ListEventsByFilter :many
SELECT * FROM events
WHERE (classification_code = $1 OR $1 = '')
  AND (booking_id = $2 OR $2 = 0)
  AND (to_status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: GetPendingEvents :many
WITH claimed AS (
    SELECT id
    FROM events
    WHERE delivery_status = 'pending'
      AND next_attempt_at <= now()
    ORDER BY next_attempt_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE events AS e
SET next_attempt_at = $2
FROM claimed
WHERE e.id = claimed.id
RETURNING e.*;

-- name: UpdateEventDelivered :execrows
-- Claim-token guard (mirrors rollup_queue's MarkRollupProcessed): only the
-- worker whose lease (next_attempt_at set at claim time) is still current may
-- record a delivery outcome. Without this, a worker whose callback outlived
-- its lease could overwrite the result written by the worker that re-claimed
-- the event (e.g. a stale 'delivered' clobbering a legitimate retry bump).
UPDATE events
SET delivery_status = 'delivered', delivered_at = now()
WHERE id = $1 AND next_attempt_at = $2;

-- name: UpdateEventRetry :execrows
-- Claim-token guard: see UpdateEventDelivered.
UPDATE events
SET attempts = attempts + 1,
    delivery_status = CASE
        WHEN attempts + 1 >= max_attempts THEN 'dead'
        ELSE 'pending'
    END,
    next_attempt_at = CASE
        WHEN attempts + 1 >= max_attempts THEN next_attempt_at
        ELSE $2
    END
WHERE id = $1 AND next_attempt_at = $3;

-- name: UpdateEventDead :execrows
-- Claim-token guard: see UpdateEventDelivered.
UPDATE events
SET delivery_status = 'dead'
WHERE id = $1 AND next_attempt_at = $2;

-- name: GetEventByUID :one
SELECT * FROM events WHERE uid = $1;

-- name: GetEventUIDByID :one
SELECT uid FROM events WHERE id = $1;

-- name: GetEventIDByUID :one
SELECT id FROM events WHERE uid = $1;
