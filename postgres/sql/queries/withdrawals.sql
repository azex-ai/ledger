-- name: InsertWithdrawal :one
INSERT INTO withdrawals (account_holder, currency_id, amount, channel_name, idempotency_key, metadata, review_required, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, account_holder, currency_id, amount, status, channel_name, channel_ref, reservation_id, journal_id, idempotency_key, metadata, review_required, expires_at, created_at, updated_at;

-- name: GetWithdrawal :one
SELECT id, account_holder, currency_id, amount, status, channel_name, channel_ref, reservation_id, journal_id, idempotency_key, metadata, review_required, expires_at, created_at, updated_at
FROM withdrawals WHERE id = $1;

-- name: GetWithdrawalForUpdate :one
SELECT id, account_holder, currency_id, amount, status, channel_name, channel_ref, reservation_id, journal_id, idempotency_key, metadata, review_required, expires_at, created_at, updated_at
FROM withdrawals WHERE id = $1 FOR UPDATE;

-- name: UpdateWithdrawalStatus :exec
UPDATE withdrawals SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalReservation :exec
UPDATE withdrawals SET reservation_id = $2, status = 'reserved', updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalProcess :exec
UPDATE withdrawals SET status = 'processing', channel_ref = $2, updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalConfirm :exec
UPDATE withdrawals SET status = 'confirmed', journal_id = $2, updated_at = now() WHERE id = $1;

-- name: ListWithdrawalsByAccount :many
SELECT id, account_holder, currency_id, amount, status, channel_name, channel_ref, reservation_id, journal_id, idempotency_key, metadata, review_required, expires_at, created_at, updated_at
FROM withdrawals
WHERE account_holder = $1
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;
