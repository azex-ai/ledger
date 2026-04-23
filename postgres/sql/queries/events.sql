-- name: InsertEvent :one
INSERT INTO events (
    classification_code, booking_id, account_holder, currency_id,
    from_status, to_status, amount, settled_amount, journal_id,
    metadata, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: ListEventsByFilter :many
SELECT * FROM events
WHERE (classification_code = $1 OR $1 = '')
  AND (booking_id = $2 OR $2 = 0)
  AND (to_status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: GetPendingEvents :many
SELECT * FROM events
WHERE delivery_status = 'pending'
  AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: UpdateEventDelivered :exec
UPDATE events
SET delivery_status = 'delivered', delivered_at = now()
WHERE id = $1;

-- name: UpdateEventRetry :exec
UPDATE events
SET delivery_status = 'pending',
    attempts = attempts + 1,
    next_attempt_at = $2
WHERE id = $1;

-- name: UpdateEventDead :exec
UPDATE events
SET delivery_status = 'dead'
WHERE id = $1;
