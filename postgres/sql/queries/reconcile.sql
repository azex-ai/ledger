-- Reconciliation queries for the full 10-check suite.
-- All queries are read-only (no mutations).

-- name: ReconcileOrphanEntriesCount :one
-- Check #3: count entries whose journal_id does not match any journal row.
SELECT COUNT(*)::bigint AS orphan_count
FROM journal_entries je
LEFT JOIN journals j ON je.journal_id = j.id
WHERE j.id IS NULL;

-- name: ReconcileOrphanEntriesSample :many
-- Fetch a small sample of orphan entries for the Finding descriptions.
SELECT je.id::bigint AS entry_id, je.journal_id
FROM journal_entries je
LEFT JOIN journals j ON je.journal_id = j.id
WHERE j.id IS NULL
ORDER BY je.id
LIMIT 10;

-- name: ReconcileAccountingEquation :many
-- Check #4: per-currency, per-classification balance sums with normal_side.
-- Returns one row per (currency_id, classification_id) so the caller can
-- compute the equation A = L + E per currency.
SELECT
  je.currency_id,
  je.classification_id,
  c.normal_side,
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit'  THEN je.amount ELSE 0 END), 0)::numeric AS total_debit,
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)::numeric AS total_credit
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
GROUP BY je.currency_id, je.classification_id, c.normal_side
ORDER BY je.currency_id, je.classification_id;

-- name: ReconcileSettlementNetting :many
-- Check #5: per-currency net balance of a named classification,
-- excluding entries within the given window (in minutes) to tolerate in-flight transactions.
-- Returns only rows where the net is non-zero (violations).
SELECT
  je.currency_id,
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit'  THEN je.amount ELSE -je.amount END), 0)::numeric AS net_balance
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE c.code = sqlc.arg(classification_code)::text
  AND je.created_at < now() - (sqlc.arg(window_minutes)::int * INTERVAL '1 minute')
GROUP BY je.currency_id
HAVING COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE -je.amount END), 0) != 0
ORDER BY je.currency_id;

-- name: ReconcileNonNegativeBalances :many
-- Check #6: every positive holder × classification with a negative computed balance.
-- "Positive holder" = user account (holder > 0).
SELECT
  je.account_holder,
  je.currency_id,
  je.classification_id,
  c.normal_side,
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit'  THEN je.amount ELSE 0 END), 0)::numeric AS total_debit,
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)::numeric AS total_credit
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.account_holder > 0
GROUP BY je.account_holder, je.currency_id, je.classification_id, c.normal_side
HAVING (
  CASE
    WHEN MIN(c.normal_side) = 'debit'
      THEN COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE 0 END), 0)
         - COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)
    ELSE
         COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)
         - COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE 0 END), 0)
  END
) < 0
ORDER BY je.account_holder, je.classification_id
LIMIT sqlc.arg(page_limit)::int;

-- name: ReconcileOrphanReservations :many
-- Check #7: reservations whose journal_id references a non-existent journal.
-- reservations.journal_id is NOT NULL DEFAULT 0 (sentinel); treat 0 as "no journal".
SELECT
  r.id,
  r.account_holder,
  r.currency_id,
  r.status,
  r.journal_id
FROM reservations r
WHERE r.journal_id != 0
  AND NOT EXISTS (SELECT 1 FROM journals j WHERE j.id = r.journal_id)
ORDER BY r.id
LIMIT 100;

-- name: ReconcileStaleRollupItems :many
-- Check #10: rollup_queue items that are claimed but the claim has expired,
-- older than @threshold_minutes minutes. These indicate stuck workers.
SELECT
  q.id,
  q.account_holder,
  q.currency_id,
  q.classification_id,
  q.claimed_until,
  q.failed_attempts
FROM rollup_queue q
WHERE q.processed_at IS NULL
  AND q.claimed_until IS NOT NULL
  AND q.claimed_until < now() - (sqlc.arg(threshold_minutes)::int * INTERVAL '1 minute')
ORDER BY q.claimed_until
LIMIT 100;

-- name: ReconcileDuplicateIdempotencyKeys :many
-- Check #9: defensive scan for journals with duplicate idempotency_key values.
-- The UNIQUE index should prevent this, but we verify the invariant explicitly.
SELECT
  idempotency_key,
  COUNT(*)::bigint AS occurrences,
  MIN(id)::bigint AS first_id,
  MAX(id)::bigint AS last_id
FROM journals
GROUP BY idempotency_key
HAVING COUNT(*) > 1
ORDER BY occurrences DESC
LIMIT 50;
