-- name: CreateClassification :one
INSERT INTO classifications (code, name, normal_side, is_system, lifecycle)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: DeactivateClassification :exec
UPDATE classifications SET is_active = false WHERE id = $1;

-- name: GetClassification :one
SELECT * FROM classifications WHERE id = $1;

-- name: GetClassificationByCode :one
SELECT * FROM classifications WHERE code = $1;

-- name: ListClassifications :many
SELECT * FROM classifications
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;

-- name: CreateJournalType :one
INSERT INTO journal_types (code, name)
VALUES ($1, $2)
RETURNING id, code, name, is_active, created_at;

-- name: DeactivateJournalType :exec
UPDATE journal_types SET is_active = false WHERE id = $1;

-- name: ListJournalTypes :many
SELECT id, code, name, is_active, created_at
FROM journal_types
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;
