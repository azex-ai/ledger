-- name: InsertJournal :one
INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, created_at;

-- name: InsertJournalEntry :one
INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at;

-- name: GetJournal :one
SELECT id, journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, created_at
FROM journals
WHERE id = $1;

-- name: GetJournalByIdempotencyKey :one
SELECT id, journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, created_at
FROM journals
WHERE idempotency_key = $1;

-- name: ListJournalEntries :many
SELECT id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at
FROM journal_entries
WHERE journal_id = $1
ORDER BY id;

-- name: ListEntriesByAccount :many
SELECT id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at
FROM journal_entries
WHERE account_holder = $1 AND currency_id = $2
  AND id > sqlc.arg(cursor_id)::bigint
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: SumEntriesSinceCheckpoint :many
SELECT
  classification_id,
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND id > sqlc.arg(since_entry_id)::bigint
GROUP BY classification_id, entry_type;

-- name: SumGlobalDebitCredit :many
SELECT
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
GROUP BY entry_type;

-- name: SumEntriesByAccountClassification :many
SELECT
  classification_id,
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
GROUP BY classification_id, entry_type;
