-- name: InsertReservation :one
INSERT INTO reservations (account_holder, currency_id, reserved_amount, idempotency_key, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at;

-- name: GetReservation :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE id = $1;

-- name: GetReservationByIdempotencyKey :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE idempotency_key = $1;

-- name: GetReservationForUpdate :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE id = $1 FOR UPDATE;

-- name: UpdateReservationStatus :exec
UPDATE reservations SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateReservationSettle :exec
UPDATE reservations SET status = 'settled', settled_amount = $2, journal_id = $3, updated_at = now() WHERE id = $1;

-- name: ListReservationsByAccount :many
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations
WHERE (sqlc.arg(account_holder)::bigint = 0 OR account_holder = sqlc.arg(account_holder))
  AND (sqlc.arg(filter_status)::text = '' OR status = sqlc.arg(filter_status))
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetExpiredReservations :many
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE status = 'active' AND expires_at < now() LIMIT $1;

-- name: SumActiveReservations :one
SELECT COALESCE(SUM(reserved_amount), 0) as total
FROM reservations
WHERE account_holder = $1 AND currency_id = $2 AND status = 'active';
