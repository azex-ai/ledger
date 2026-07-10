-- name: CreateClassification :one
INSERT INTO classifications (code, name, normal_side, is_system, lifecycle, uid, balance_role, display_label)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: SetClassificationDisplayLabelIfEmpty :exec
-- Seeds the user-facing label only when unset — presets re-install must not
-- clobber an operator's override (same expand-safe stance as balance_role).
UPDATE classifications SET display_label = $2 WHERE uid = $1 AND display_label = '';

-- name: SetClassificationBalanceRole :exec
-- Role upgrades are expand-safe ('' -> role); anything else is a semantic
-- change the caller must guard (presets only upgrade from '').
UPDATE classifications SET balance_role = $2 WHERE uid = $1;

-- name: SetClassificationLifecycleIfEmpty :exec
-- Seeds a classification's lifecycle only when unset ('{}') -- for rows that
-- predate the lifecycle column and were never assigned one (e.g. migration
-- 011's seed 'deposit'/'withdraw' rows). Same expand-safe stance as
-- SetClassificationDisplayLabelIfEmpty/SetClassificationBalanceRole: presets
-- re-install must never clobber a lifecycle an operator has since customized.
UPDATE classifications SET lifecycle = $2 WHERE uid = $1 AND lifecycle = '{}'::jsonb;

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
INSERT INTO journal_types (code, name, uid, display_label)
VALUES ($1, $2, $3, $4)
RETURNING id, code, name, is_active, created_at, uid, display_label;

-- name: SetJournalTypeDisplayLabelIfEmpty :exec
-- See SetClassificationDisplayLabelIfEmpty.
UPDATE journal_types SET display_label = $2 WHERE uid = $1 AND display_label = '';

-- name: GetJournalTypeByCode :one
SELECT id, code, name, is_active, created_at, uid, display_label
FROM journal_types
WHERE code = $1;

-- name: DeactivateJournalType :exec
UPDATE journal_types SET is_active = false WHERE uid = $1;

-- name: ListJournalTypes :many
SELECT id, code, name, is_active, created_at, uid, display_label
FROM journal_types
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY id;

-- name: ListClassificationDims :many
-- Full config-table scan for the in-process id<->uid dimension cache.
SELECT id, uid, code, normal_side, balance_role FROM classifications;

-- name: ListJournalTypeDims :many
SELECT id, uid, code FROM journal_types;
