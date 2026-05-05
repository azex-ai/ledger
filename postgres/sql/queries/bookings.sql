-- name: InsertBooking :one
INSERT INTO bookings (
    classification_id, account_holder, currency_id, amount, status,
    channel_name, idempotency_key, metadata, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetBooking :one
SELECT * FROM bookings WHERE id = $1;

-- name: GetBookingForUpdate :one
SELECT * FROM bookings WHERE id = $1 FOR UPDATE;

-- name: GetBookingByIdempotencyKey :one
SELECT * FROM bookings WHERE idempotency_key = $1;

-- name: UpdateBookingTransition :exec
UPDATE bookings
SET status = $2, channel_ref = $3, settled_amount = $4,
    journal_id = $5, metadata = $6, updated_at = now()
WHERE id = $1;

-- name: LinkBookingJournal :one
UPDATE bookings
SET journal_id = $2, updated_at = now()
WHERE id = $1
  AND journal_id IS NULL
RETURNING *;

-- name: ListBookingsByFilter :many
SELECT * FROM bookings
WHERE (account_holder = $1 OR $1 = 0)
  AND (classification_id = $2 OR $2 = 0)
  AND (status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: ListExpiredBookings :many
SELECT b.*
FROM bookings b
INNER JOIN classifications c ON c.id = b.classification_id
WHERE b.expires_at != 'epoch'
  AND b.expires_at < now()
  AND COALESCE(c.lifecycle -> 'transitions' -> b.status, '[]'::jsonb) ? 'expired'
LIMIT $1;
