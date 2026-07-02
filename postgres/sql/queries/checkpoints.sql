-- name: GetBalanceCheckpoint :one
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3;

-- name: UpsertBalanceCheckpoint :exec
-- Monotonic: only advance the checkpoint. last_entry_id is non-decreasing
-- (journal_entries.id is append-only), so a writer carrying an OLDER snapshot
-- (lower last_entry_id) must never overwrite a fresher checkpoint. Without this
-- guard, two rollup workers processing the same dimension concurrently (possible
-- once an enqueue re-dirties a claimed row) could let the slower/older writer
-- regress the checkpoint. Balances stay correct either way via the delta, but
-- the guard keeps the checkpoint from going stale.
INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_holder, currency_id, classification_id)
DO UPDATE SET balance = $4, last_entry_id = $5, last_entry_at = $6, updated_at = now()
WHERE balance_checkpoints.last_entry_id < EXCLUDED.last_entry_id;

-- name: GetBalanceCheckpoints :many
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2;

-- name: EnqueueRollup :exec
-- Re-dirty on conflict: if an unprocessed row already exists for the dimension
-- (idle OR currently claimed by a worker), reset its claim. This signals "new
-- unmaterialized work arrived". Combined with MarkRollupProcessed's claim guard,
-- an enqueue that lands while the worker is mid-processing forces a reprocess
-- instead of being silently coalesced away. (Balances stay correct via the delta
-- regardless; this keeps the checkpoint from lagging indefinitely.)
INSERT INTO rollup_queue (account_holder, currency_id, classification_id)
VALUES ($1, $2, $3)
ON CONFLICT (account_holder, currency_id, classification_id) WHERE processed_at IS NULL
DO UPDATE SET claimed_until = NULL;

-- name: DequeueRollupBatch :many
-- Skip items that have failed too many times (failed_attempts >= 10) — they
-- need operator attention, not another retry loop.
WITH claimed AS (
    SELECT id
    FROM rollup_queue
    WHERE processed_at IS NULL
      AND (claimed_until IS NULL OR claimed_until < now())
      AND failed_attempts < 10
    ORDER BY created_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE rollup_queue AS q
SET claimed_until = $2
FROM claimed
WHERE q.id = claimed.id
RETURNING q.id, q.account_holder, q.currency_id, q.classification_id, q.created_at, q.claimed_until;

-- name: MarkRollupProcessed :execrows
-- Claim-token guard: only mark processed if THIS worker still owns the claim —
-- claimed_until must still equal the exact token we set at dequeue ($2, the value
-- returned from DequeueRollupBatch). If a concurrent EnqueueRollup re-dirtied the
-- row (claimed_until = NULL) or another worker re-claimed it (different
-- claimed_until) while we were processing, this affects 0 rows and the row stays
-- pending for its rightful owner — so an enqueue during processing is never lost,
-- and a stale worker can never mark a claim it no longer owns. Returns rows
-- affected so the caller can distinguish "marked" from "claim lost".
UPDATE rollup_queue
SET processed_at = now(), claimed_until = NULL
WHERE id = $1 AND claimed_until = $2;

-- name: ReleaseRollupClaim :exec
-- Release the claim *and* bump failed_attempts so a permanently-failing item
-- can be detected and excluded from future batches (see DequeueRollupBatch).
-- Claim-token scoped ($2): only the worker that owns the current claim releases
-- it. If the row was re-dirtied (claimed_until = NULL) or re-claimed by another
-- worker, this no-ops — a stale worker must not bump failed_attempts on work it
-- no longer owns (else repeated races could falsely exhaust failed_attempts and
-- exclude a live dimension).
UPDATE rollup_queue
SET claimed_until = NULL,
    failed_attempts = failed_attempts + 1
WHERE id = $1
  AND processed_at IS NULL
  AND claimed_until = $2;

-- name: CountPendingRollups :one
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL;

-- name: GetCheckpointMaxAgeSeconds :one
SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(updated_at)))::bigint, 0)::bigint as max_age_seconds
FROM balance_checkpoints;

-- name: GetMaxEntryID :one
SELECT COALESCE(MAX(id), 0)::bigint as max_id FROM journal_entries;

-- name: GetMaxEntryForAccountCurrencySince :one
SELECT
  COALESCE(MAX(id), 0)::bigint AS max_entry_id,
  COALESCE(MAX(created_at), 'epoch'::timestamptz) AS max_entry_at
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND id > $3;

-- name: SumGlobalDebitCreditByCurrency :many
SELECT
  currency_id,
  entry_type,
  COALESCE(SUM(amount), 0)::numeric AS total
FROM journal_entries
GROUP BY currency_id, entry_type
ORDER BY currency_id, entry_type;

-- name: ListBalancesAt :many
-- As-of cutoff is applied on effective_at (business date), not created_at
-- (write date) — see docs/plans/2026-07-02-financial-core-hardening-design.md §1.
SELECT
  je.account_holder,
  je.currency_id,
  je.classification_id,
  COALESCE(SUM(
    CASE
      WHEN c.normal_side = 'credit' AND je.entry_type = 'credit' THEN je.amount
      WHEN c.normal_side = 'credit' AND je.entry_type = 'debit' THEN -je.amount
      WHEN je.entry_type = 'debit' THEN je.amount
      ELSE -je.amount
    END
  ), 0)::numeric AS balance
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.effective_at < $1
GROUP BY je.account_holder, je.currency_id, je.classification_id
ORDER BY je.account_holder, je.currency_id, je.classification_id;

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
