-- name: CreateClassification :one
INSERT INTO classifications (code, name, normal_side, is_system, lifecycle, uid, balance_role)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: SetClassificationBalanceRole :exec
-- Role upgrades are expand-safe ('' -> role); anything else is a semantic
-- change the caller must guard (presets only upgrade from '').
UPDATE classifications SET balance_role = $2 WHERE uid = $1;

-- name: DeactivateClassification :exec
UPDATE classifications SET is_active = false WHERE uid = $1;

-- name: GetClassification :one
SELECT * FROM classifications WHERE id = $1;

-- name: GetClassificationByCode :one
SELECT * FROM classifications WHERE code = $1;

-- name: ListClassifications :many
SELECT * FROM classifications
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;

-- name: CreateJournalType :one
INSERT INTO journal_types (code, name, uid)
VALUES ($1, $2, $3)
RETURNING id, code, name, is_active, created_at, uid;

-- name: GetJournalTypeByCode :one
SELECT id, code, name, is_active, created_at, uid
FROM journal_types
WHERE code = $1;

-- name: DeactivateJournalType :exec
UPDATE journal_types SET is_active = false WHERE uid = $1;

-- name: ListJournalTypes :many
SELECT id, code, name, is_active, created_at, uid
FROM journal_types
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;

-- name: ListClassificationDims :many
-- Full config-table scan for the in-process id<->uid dimension cache.
SELECT id, uid, code, normal_side, balance_role FROM classifications;

-- name: ListJournalTypeDims :many
SELECT id, uid, code FROM journal_types;
