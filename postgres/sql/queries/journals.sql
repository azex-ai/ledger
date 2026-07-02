-- name: InsertJournal :one
INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, event_id, effective_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: InsertJournalEntry :one
-- Column order in RETURNING matches the table's physical column order
-- (effective_at was appended after created_at in migration 025) so sqlc
-- matches the generated row to the JournalEntry model instead of minting a
-- distinct one-off row type.
INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at;

-- name: GetJournal :one
SELECT * FROM journals WHERE id = $1;

-- name: GetJournalByIdempotencyKey :one
SELECT * FROM journals WHERE idempotency_key = $1;

-- name: GetReversalByOriginalJournalID :one
SELECT * FROM journals WHERE reversal_of = $1;

-- name: ListJournalEntries :many
-- Column order matches the table's physical order (see InsertJournalEntry).
SELECT id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at
FROM journal_entries
WHERE journal_id = $1
ORDER BY id;

-- name: ListEntriesByAccount :many
-- Column order matches the table's physical order (see InsertJournalEntry).
SELECT id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at
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

-- name: DistinctClassificationsForAccount :many
SELECT DISTINCT classification_id
FROM journal_entries
WHERE account_holder = $1 AND currency_id = $2
ORDER BY classification_id;

-- name: SumEntriesSinceForClassification :many
SELECT
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND classification_id = $3
  AND id > sqlc.arg(since_entry_id)::bigint
GROUP BY entry_type;

-- name: ListJournalsCursor :many
SELECT * FROM journals
WHERE id > sqlc.arg(cursor_id)::bigint
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

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

-- name: VerifyJournalBalanced :one
-- Returns the first currency_id that does not net to zero across the journal's
-- entries, or NULL if the journal is balanced. Run inside the same transaction
-- as the entry inserts; replaces the per-row CONSTRAINT TRIGGER dropped in 018.
SELECT currency_id
FROM journal_entries
WHERE journal_id = $1
GROUP BY currency_id
HAVING SUM(CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END) <> 0
LIMIT 1;

-- name: AcquireBalanceLock :exec
-- Take a transaction-scoped advisory lock keyed on (holder, currency_id) so
-- concurrent reserves and journal posts that touch the same pair serialize.
-- The caller passes a stable composite text key (e.g. "balance:<holder>:<currency_id>");
-- hash collisions only reduce concurrency, they do not affect correctness.
-- Single-arg bigint form is used so int64 ids do not need to be narrowed to int32.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- name: AcquireIdempotencyLock :exec
-- Serialize concurrent requests that present the same idempotency key, even if
-- they touch different account dimensions. Collisions in the hash only reduce
-- concurrency; they do not affect correctness.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));
