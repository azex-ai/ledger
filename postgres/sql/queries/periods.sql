-- name: InsertPeriodClose :one
-- Append-only: latest-row-wins semantics come from ordering by created_at in
-- GetActivePeriodClose, not from any uniqueness constraint here.
INSERT INTO period_closes (close_before, note, actor_id, uid)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetActivePeriodClose :one
-- The active close line is the most recently created row. Returns
-- pgx.ErrNoRows when the period has never been closed.
SELECT * FROM period_closes
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListPeriodCloses :many
SELECT * FROM period_closes
ORDER BY created_at DESC, id DESC
LIMIT $1;
