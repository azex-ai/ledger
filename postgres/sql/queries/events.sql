-- name: InsertEvent :one
INSERT INTO events (
    classification_code, booking_id, account_holder, currency_id,
    from_status, to_status, amount, settled_amount, journal_id,
    metadata, occurred_at, actor_id, source
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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

-- name: UpdateEventDelivered :exec
UPDATE events
SET delivery_status = 'delivered', delivered_at = now()
WHERE id = $1;

-- name: UpdateEventRetry :exec
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
WHERE id = $1;

-- name: UpdateEventDead :exec
UPDATE events
SET delivery_status = 'dead'
WHERE id = $1;
