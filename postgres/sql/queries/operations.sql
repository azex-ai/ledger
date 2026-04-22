-- name: InsertOperation :one
INSERT INTO operations (
    classification_id, account_holder, currency_id, amount, status,
    channel_name, idempotency_key, metadata, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetOperation :one
SELECT * FROM operations WHERE id = $1;

-- name: GetOperationForUpdate :one
SELECT * FROM operations WHERE id = $1 FOR UPDATE;

-- name: GetOperationByIdempotencyKey :one
SELECT * FROM operations WHERE idempotency_key = $1;

-- name: UpdateOperationTransition :exec
UPDATE operations
SET status = $2, channel_ref = $3, settled_amount = $4,
    journal_id = $5, metadata = $6, updated_at = now()
WHERE id = $1;

-- name: ListOperationsByFilter :many
SELECT * FROM operations
WHERE (account_holder = $1 OR $1 = 0)
  AND (classification_id = $2 OR $2 = 0)
  AND (status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: ListExpiredOperations :many
SELECT * FROM operations
WHERE expires_at != 'epoch'
  AND expires_at < now()
  AND status NOT IN ('confirmed', 'failed', 'expired', 'settled', 'released')
LIMIT $1;
