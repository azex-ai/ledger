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

-- name: RebuildBalanceCheckpoint :exec
-- Trusted-operator overwrite: unlike UpsertBalanceCheckpoint's monotonic
-- guard (last_entry_id can only advance), this unconditionally replaces the
-- row. A poisoned checkpoint can have an arbitrary last_entry_id — including
-- one higher than the true watermark — so the monotonic guard that protects
-- the rollup worker's normal path would refuse the very fix meant to correct
-- it. Only CheckpointIntegrityStore.RebuildCheckpoint calls this, and only
-- after taking the (holder, currency_id) advisory lock and confirming no
-- rollup_queue item is pending for the dimension (CountPendingRollupForDimension) —
-- see docs/plans/2026-08-21-tamper-evident-ledger-design.md §4.
INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_holder, currency_id, classification_id)
DO UPDATE SET balance = $4, last_entry_id = $5, last_entry_at = $6, updated_at = now();

-- name: GetBalanceCheckpoints :many
SELECT account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at, updated_at
FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2;

-- name: ListComputedBalancesForHolders :many
-- Computes checkpoint + delta for every populated classification in one
-- snapshot-consistent query. This is the batch primitive behind GetBalances,
-- BatchGetBalances, and role breakdowns; callers must not loop GetBalance.
--
-- H-M6. Both halves used to read the holder's WHOLE history, which cancelled
-- out the point of storing a checkpoint at all. Measured on a 24-partition
-- fixture (postgres.TestBalanceRead_CostDoesNotGrowWithHistory): the old
-- shape touched 42,240 entry rows to answer a five-entry delta for a holder
-- with 9,600 entries, and exactly 10x that for 10x the history. This shape
-- touches 102, whatever the history. Two causes, one per half:
--
--   * `SELECT DISTINCT ... FROM journal_entries` to find which
--     classifications the holder has touched: a full index-only scan of the
--     holder's history, in every partition, to produce (here) three rows.
--     It is now a "loose index scan" -- a recursive walk of
--     min(classification_id) > previous over the same
--     (account_holder, currency_id, classification_id, id) index, which
--     visits one index tuple per (dimension, partition) instead of all of
--     them. Same set by construction: repeatedly taking the smallest
--     classification_id greater than the last one enumerates exactly the
--     distinct values present.
--
--   * the delta as a LEFT JOIN + GROUP BY: `je.id > cp.last_entry_id` takes
--     its bound from a joined row, so the planner had no constant to index
--     on and chose a hash join over a sequential Append across every
--     partition -- reading every entry of every holder, then discarding
--     almost all of them. As a LATERAL it is one index range scan per
--     dimension with all four index columns bound, so it reads the delta and
--     nothing else. Same value: SUM over an empty set is NULL either way,
--     and COALESCE turns it into 0, and the GROUP BY disappears because the
--     dimension walk already yields one row per (holder, classification).
--
-- This is not a new pattern in this repository -- platform_balances.sql has
-- always computed its checkpoint+delta with exactly this LATERAL, and says
-- why in its header. Two queries implementing one formula in two shapes, the
-- slow one on the hot path, is what the audit found.
--
-- Still true after this change (docs/INVARIANTS.md I-5): there is no
-- predicate on created_at, so NO partition pruning happens and the constant
-- factor grows by one partition per month. Adding a created_at lower bound
-- derived from balance_checkpoints.last_entry_at is safe only if entry id is
-- monotonic with created_at, which migration 008 guarantees by ACL and a
-- single sequence rather than structurally -- deferred deliberately, not
-- overlooked.
WITH RECURSIVE populated AS (
    SELECT
        h.account_holder,
        (
          SELECT MIN(je.classification_id)
          FROM journal_entries je
          WHERE je.account_holder = h.account_holder
            AND je.currency_id = sqlc.arg(currency_id)::bigint
        ) AS classification_id
    FROM unnest(sqlc.arg(holder_ids)::bigint[]) AS h(account_holder)
  UNION ALL
    SELECT
        p.account_holder,
        (
          SELECT MIN(je.classification_id)
          FROM journal_entries je
          WHERE je.account_holder = p.account_holder
            AND je.currency_id = sqlc.arg(currency_id)::bigint
            AND je.classification_id > p.classification_id
        )
    FROM populated p
    WHERE p.classification_id IS NOT NULL
)
SELECT
    p.account_holder::bigint AS account_holder,
    c.id AS classification_id,
    c.uid AS classification_uid,
    c.balance_role,
    (COALESCE(cp.balance, 0::numeric) + COALESCE(d.delta, 0::numeric))::numeric AS balance
FROM populated p
JOIN classifications c ON c.id = p.classification_id
LEFT JOIN balance_checkpoints cp
  ON cp.account_holder = p.account_holder
 AND cp.currency_id = sqlc.arg(currency_id)::bigint
 AND cp.classification_id = p.classification_id
LEFT JOIN LATERAL (
    SELECT SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)) AS delta
    FROM journal_entries je
    WHERE je.account_holder = p.account_holder
      AND je.currency_id = sqlc.arg(currency_id)::bigint
      AND je.classification_id = p.classification_id
      AND je.id > COALESCE(cp.last_entry_id, 0)
) d ON TRUE
WHERE p.classification_id IS NOT NULL
ORDER BY p.account_holder, c.id;

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
-- Excludes items that have exhausted their retry budget (failed_attempts >=
-- 10, the same threshold DequeueRollupBatch stops dequeuing at) -- see
-- CountStuckRollups for those. Before this exclusion, a single stuck item
-- kept this gauge (and the alert built on it) pinned above zero forever,
-- indistinguishable from ordinary queue depth that is still being drained
-- (B-m10, working-agreements §3: a permanently-stuck item and a healthy
-- backlog must not look the same).
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL AND failed_attempts < 10;

-- name: CountStuckRollups :one
-- Items that exhausted their retry budget (failed_attempts >= 10) and will
-- never be dequeued again without manual intervention (see
-- ResetRollupClaim / docs/RUNBOOK.md "stuck rollup items", B-m10).
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL AND failed_attempts >= 10;

-- name: ResetRollupClaim :execrows
-- Operator escape hatch for a stuck rollup_queue item (B-m10): clears the
-- claim and resets failed_attempts so DequeueRollupBatch's `failed_attempts
-- < 10` filter picks it up again on the next tick. No claim-token check --
-- unlike ReleaseRollupClaim/MarkRollupProcessed, this is an explicit,
-- out-of-band operator action (via cmd/ledger-cli), not something a worker
-- calls as part of its own processing loop.
UPDATE rollup_queue
SET claimed_until = NULL,
    failed_attempts = 0
WHERE id = $1
  AND processed_at IS NULL;

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
  COALESCE(SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)), 0)::numeric AS balance
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.effective_at < $1
GROUP BY je.account_holder, je.currency_id, je.classification_id
ORDER BY je.account_holder, je.currency_id, je.classification_id;

-- name: GetMaxEntryCreatedAtForDimensionBefore :one
-- Returns the latest created_at among journal_entries for exactly one
-- (account_holder, currency_id, classification_id) dimension whose
-- effective_at is before cutoff, or the epoch sentinel when none exist.
--
-- Used to detect a stale balance_snapshots row: a snapshot is computed once,
-- from whatever entries existed at that moment (service/snapshot.go's
-- CreateDailySnapshot). Nothing re-triggers it when a later write
-- retroactively backdates (effective_at < cutoff) into an already-
-- snapshotted business date — this value being later than the snapshot
-- row's own created_at is exactly that condition. See
-- docs/audits/2026-08-25-financial-engineering/financial-correctness.md
-- Major #2 ("effective_at 回溯记账不会让已写入的历史快照失效").
SELECT COALESCE(MAX(created_at), 'epoch'::timestamptz) AS max_created_at
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND classification_id = $3
  AND effective_at < $4;

-- name: GetMaxEntryCreatedAtForHolderCurrencyBefore :one
-- The dimension-agnostic sibling of GetMaxEntryCreatedAtForDimensionBefore:
-- the latest created_at among ALL of one (account_holder, currency_id)'s
-- entries whose effective_at is before cutoff.
--
-- The per-dimension version can only ask "was this row invalidated", which is
-- unanswerable for a dimension that HAS no row. Backdating a journal into an
-- already-snapshotted date can introduce a classification that did not exist
-- when the snapshot ran, and that dimension is then absent from the cached
-- set entirely -- an as-of read silently short by the whole position rather
-- than wrong by some amount. A missing row is harder to notice than a wrong
-- number, and the sparse snapshot mode (snapshot_extra_store.go writes no row
-- when a balance did not change) makes "no row" the normal case. Comparing
-- this value against the OLDEST snapshot row of the date tells the reader
-- that something backdated landed at all, which is when the whole dimension
-- set has to be recomputed rather than patched row by row.
SELECT COALESCE(MAX(created_at), 'epoch'::timestamptz) AS max_created_at
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND effective_at < $3;

-- name: ListBalancesAtForHolderCurrency :many
-- ListBalancesAt narrowed to one (account_holder, currency_id). The unscoped
-- version aggregates the whole table and is filtered in Go, which is fine for
-- the rollup worker's periodic pass and wasteful on a per-request as-of read.
SELECT
  je.classification_id,
  COALESCE(SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)), 0)::numeric AS balance
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.account_holder = $1
  AND je.currency_id = $2
  AND je.effective_at < $3
GROUP BY je.classification_id
ORDER BY je.classification_id;

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
