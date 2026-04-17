-- name: CreateCurrency :one
INSERT INTO currencies (code, name)
VALUES ($1, $2)
RETURNING id, code, name;

-- name: GetCurrency :one
SELECT id, code, name
FROM currencies
WHERE id = $1;

-- name: ListCurrencies :many
SELECT id, code, name
FROM currencies
ORDER BY id;
