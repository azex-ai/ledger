-- name: InsertBooking :one
INSERT INTO bookings (
    classification_id, account_holder, currency_id, amount, status,
    channel_name, idempotency_key, metadata, expires_at, uid
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
ORDER BY b.expires_at ASC
LIMIT $1;

-- name: GetBookingByUID :one
SELECT * FROM bookings WHERE uid = $1;

-- name: GetBookingForUpdateByUID :one
SELECT * FROM bookings WHERE uid = $1 FOR UPDATE;

-- name: GetBookingUIDByID :one
SELECT uid FROM bookings WHERE id = $1;

-- name: ListBookingsByDepositIdentity :many
-- Every booking claiming one on-chain transfer log, by the identity the
-- deposit path derives its idempotency key from (I-20). Served by migration
-- 032's uq_bookings_deposit_identity, which also makes more than one row
-- impossible for the honest writer -- this query is what lets the
-- application say WHICH booking already holds the log rather than only
-- refusing (I-71), and what still answers correctly on a deployment that has
-- not applied 032 yet.
SELECT * FROM bookings
WHERE metadata->>'chain_id'  = @chain_id::text
  AND metadata->>'tx_hash'   = @tx_hash::text
  AND metadata->>'txlog_seq' = @txlog_seq::text
ORDER BY id;
