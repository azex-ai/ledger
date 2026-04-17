-- name: CreateClassification :one
INSERT INTO classifications (code, name, normal_side, is_system)
VALUES ($1, $2, $3, $4)
RETURNING id, code, name, normal_side, is_system, is_active, created_at;

-- name: DeactivateClassification :exec
UPDATE classifications SET is_active = false WHERE id = $1;

-- name: ListClassifications :many
SELECT id, code, name, normal_side, is_system, is_active, created_at
FROM classifications
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
