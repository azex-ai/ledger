-- name: GetBalanceCheckpoint :one
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3;

-- name: UpsertBalanceCheckpoint :exec
INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_holder, currency_id, classification_id)
DO UPDATE SET balance = $4, last_entry_id = $5, last_entry_at = $6, updated_at = now();

-- name: GetBalanceCheckpoints :many
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2;

-- name: EnqueueRollup :exec
INSERT INTO rollup_queue (account_holder, currency_id, classification_id)
VALUES ($1, $2, $3)
ON CONFLICT (account_holder, currency_id, classification_id) DO NOTHING;

-- name: DequeueRollupBatch :many
SELECT id, account_holder, currency_id, classification_id, processed_at, created_at
FROM rollup_queue
WHERE processed_at IS NULL
ORDER BY created_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkRollupProcessed :exec
UPDATE rollup_queue SET processed_at = now() WHERE id = $1;

-- name: CountPendingRollups :one
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL;

-- name: GetMaxEntryID :one
SELECT COALESCE(MAX(id), 0)::bigint as max_id FROM journal_entries;

-- name: ListAllBalanceCheckpoints :many
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
ORDER BY account_holder, currency_id, classification_id;

-- name: AggregateCheckpointsByClassification :many
SELECT
  currency_id,
  classification_id,
  COALESCE(SUM(balance), 0) as total_balance
FROM balance_checkpoints
GROUP BY currency_id, classification_id
ORDER BY currency_id, classification_id;
