-- name: InsertSnapshot :exec
INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_holder, currency_id, classification_id, snapshot_date) DO NOTHING;

-- name: GetSnapshotBalances :many
SELECT account_holder, currency_id, classification_id, snapshot_date, balance, created_at
FROM balance_snapshots
WHERE account_holder = $1 AND currency_id = $2 AND snapshot_date = $3;

-- name: ListSnapshotsByDateRange :many
SELECT account_holder, currency_id, classification_id, snapshot_date, balance, created_at
FROM balance_snapshots
WHERE account_holder = $1 AND currency_id = $2
  AND snapshot_date BETWEEN sqlc.arg(start_date) AND sqlc.arg(end_date)
ORDER BY snapshot_date;

-- name: UpsertSystemRollup :exec
INSERT INTO system_rollups (currency_id, classification_id, total_balance)
VALUES ($1, $2, $3)
ON CONFLICT (currency_id, classification_id)
DO UPDATE SET total_balance = $3, updated_at = now();

-- name: GetSystemRollups :many
SELECT currency_id, classification_id, total_balance, updated_at
FROM system_rollups ORDER BY currency_id, classification_id;
