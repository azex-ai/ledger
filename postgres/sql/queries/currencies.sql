-- name: CreateCurrency :one
INSERT INTO currencies (code, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetCurrency :one
SELECT * FROM currencies
WHERE id = $1;

-- name: DeactivateCurrency :exec
UPDATE currencies SET is_active = false WHERE id = $1;

-- name: ListCurrencies :many
SELECT * FROM currencies
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;
