-- name: InsertSnapshot :exec
INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_holder, currency_id, classification_id, snapshot_date)
DO UPDATE SET balance = EXCLUDED.balance;

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

-- name: GetLatestSnapshotBefore :one
-- Returns the most recent snapshot row for a given account before a given date.
-- Used by sparse storage to skip insert when balance is unchanged.
SELECT account_holder, currency_id, classification_id, snapshot_date, balance, created_at
FROM balance_snapshots
WHERE account_holder = $1
  AND currency_id = $2
  AND classification_id = $3
  AND snapshot_date < $4
ORDER BY snapshot_date DESC
LIMIT 1;

-- name: CountSnapshotsTotal :one
SELECT COUNT(*)::bigint FROM balance_snapshots;

-- name: GetEarliestJournalDate :one
-- Returns the earliest journal_entries created_at, or the epoch sentinel when the table is empty.
SELECT COALESCE(MIN(created_at), 'epoch'::timestamptz) AS earliest_at
FROM journal_entries;

-- name: UpsertSystemRollup :exec
INSERT INTO system_rollups (currency_id, classification_id, total_balance)
VALUES ($1, $2, $3)
ON CONFLICT (currency_id, classification_id)
DO UPDATE SET total_balance = $3, updated_at = now();

-- name: GetSystemRollups :many
SELECT currency_id, classification_id, total_balance, updated_at
FROM system_rollups ORDER BY currency_id, classification_id;
