-- name: InsertDeposit :one
INSERT INTO deposits (account_holder, currency_id, expected_amount, channel_name, idempotency_key, metadata, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at;

-- name: GetDeposit :one
SELECT id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at
FROM deposits WHERE id = $1;

-- name: GetDepositForUpdate :one
SELECT id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at
FROM deposits WHERE id = $1 FOR UPDATE;

-- name: UpdateDepositStatus :exec
UPDATE deposits SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateDepositConfirming :exec
UPDATE deposits SET status = 'confirming', channel_ref = $2, updated_at = now() WHERE id = $1;

-- name: UpdateDepositConfirm :exec
UPDATE deposits SET status = 'confirmed', actual_amount = $2, channel_ref = $3, journal_id = $4, updated_at = now() WHERE id = $1;

-- name: GetDepositByChannelRef :one
SELECT id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at
FROM deposits WHERE channel_ref = $1;

-- name: ListDepositsByAccount :many
SELECT id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at
FROM deposits
WHERE account_holder = $1
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetExpiredDeposits :many
SELECT id, account_holder, currency_id, expected_amount, actual_amount, status, channel_name, channel_ref, journal_id, idempotency_key, metadata, expires_at, created_at, updated_at
FROM deposits
WHERE status IN ('pending', 'confirming') AND expires_at IS NOT NULL AND expires_at < now()
LIMIT $1;
