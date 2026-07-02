-- name: CreateCurrency :one
INSERT INTO currencies (code, name, exponent)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetCurrency :one
SELECT * FROM currencies
WHERE id = $1;

-- name: GetCurrenciesByIDs :many
-- Batch-loads currency metadata (code, exponent, ...) for precision
-- validation on write paths — one query per journal/reserve regardless of
-- how many distinct currencies its entries touch.
SELECT * FROM currencies
WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: DeactivateCurrency :exec
UPDATE currencies SET is_active = false WHERE id = $1;

-- name: ListCurrencies :many
SELECT * FROM currencies
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;
