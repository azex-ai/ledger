-- integrity_checkpoint.sql
--
-- P2 of the integrity-hardening wave: queries that recompute balances
-- directly from journal_entries, with balance_checkpoints never appearing in
-- the FROM/JOIN clause. Checkpoint tampering therefore has zero influence on
-- any result here — this is the trusted basis behind
-- CheckpointIntegrityStore.RecomputeBalance / RebuildCheckpoint, and the
-- entries-based validation of system_rollups / balance_snapshots (M4, I-23).
--
-- See docs/plans/2026-08-21-tamper-evident-ledger-design.md §4 and
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §3.

-- name: RecomputeCheckpointFromEntries :one
-- Sums every journal_entries row for (account_holder, currency_id,
-- classification_id) from the beginning of history — balance_checkpoints is
-- never referenced. This is the trusted-recompute basis for both
-- RecomputeBalance (read-only) and RebuildCheckpoint (write path). The LEFT
-- JOIN against a single classifications row (not a WHERE on journal_entries)
-- means "classification exists but has zero entries" correctly yields
-- balance=0/last_entry_id=0, while "classification does not exist at all"
-- yields zero rows (:one then surfaces pgx.ErrNoRows, mapped to
-- core.ErrNotFound by the adapter) instead of a fabricated zero row.
SELECT
  COALESCE(SUM(CASE
    WHEN c.normal_side = 'debit'  AND je.entry_type = 'debit'  THEN je.amount
    WHEN c.normal_side = 'debit'  AND je.entry_type = 'credit' THEN -je.amount
    WHEN c.normal_side = 'credit' AND je.entry_type = 'credit' THEN je.amount
    WHEN c.normal_side = 'credit' AND je.entry_type = 'debit'  THEN -je.amount
    ELSE 0::numeric
  END), 0)::numeric AS balance,
  COALESCE(MAX(je.id), 0)::bigint AS last_entry_id,
  COALESCE(MAX(je.created_at), 'epoch'::timestamptz) AS last_entry_at
FROM classifications c
LEFT JOIN journal_entries je
  ON je.account_holder    = sqlc.arg(account_holder)::bigint
 AND je.currency_id       = sqlc.arg(currency_id)::bigint
 AND je.classification_id = c.id
WHERE c.id = sqlc.arg(classification_id)::bigint;

-- name: CountPendingRollupForDimension :one
-- Used by RebuildCheckpoint as a precondition: a rollup_queue row still
-- pending or claimed for this exact dimension means a rollup worker may have
-- already read the (possibly poisoned) checkpoint into memory and could
-- overwrite the rebuild with poisoned-base-plus-delta immediately after it
-- commits. RebuildCheckpoint refuses (core.ErrRollupPending) rather than race
-- it — the operator drains or waits for the item first.
--
-- failed_attempts < 10 mirrors checkpoints.sql's DequeueRollupBatch: once an
-- item crosses that threshold it is permanently excluded from dequeue (never
-- retried, never processed_at) and its "pending" row would otherwise count
-- forever -- meaning RebuildCheckpoint would refuse indefinitely, blocked by
-- the exact failure it exists to repair (concurrency.md Minor: "RebuildCheckpoint
-- 会被它要修的东西永久挡住"). An item this store can prove will never dequeue
-- and process cannot race a rebuild the way an in-flight one could, so it is
-- correctly excluded from "pending" here, not swept under the rug: it still
-- shows up via CountPendingRollups / the rollup_queue table itself for an
-- operator to investigate why it keeps failing.
SELECT COUNT(*)::bigint
FROM rollup_queue
WHERE account_holder = sqlc.arg(account_holder)::bigint
  AND currency_id = sqlc.arg(currency_id)::bigint
  AND classification_id = sqlc.arg(classification_id)::bigint
  AND processed_at IS NULL
  AND failed_attempts < 10;

-- name: InsertCheckpointRebuildAudit :exec
-- Durable, append-only record of every RebuildCheckpoint call (migration
-- 050). Written in the SAME transaction as RebuildBalanceCheckpoint's
-- overwrite, so the audit row and the repair are atomic: a repair can never
-- happen without leaving forensic evidence, and the evidence can never exist
-- without a corresponding repair. A non-zero drift row is the durable proof
-- a poisoned checkpoint existed -- logs can rotate or be lost, this table
-- cannot (checkpoint_rebuilds_no_update / _no_delete triggers).
INSERT INTO checkpoint_rebuilds (
    uid, account_holder, currency_id, classification_id,
    previous_balance, previous_last_entry_id,
    new_balance, new_last_entry_id, drift, actor_id
) VALUES (
    gen_random_uuid(), sqlc.arg(account_holder)::bigint, sqlc.arg(currency_id)::bigint, sqlc.arg(classification_id)::bigint,
    sqlc.arg(previous_balance)::numeric, sqlc.arg(previous_last_entry_id)::bigint,
    sqlc.arg(new_balance)::numeric, sqlc.arg(new_last_entry_id)::bigint,
    sqlc.arg(drift)::numeric, sqlc.arg(actor_id)::bigint
);

-- name: ListCheckpointRebuildsForDimension :many
-- Forensic read: every rebuild ever recorded for one dimension, newest
-- first. Used by on-call to answer "was this checkpoint ever repaired, and
-- what did it look like before" (RUNBOOK.md).
SELECT uid, account_holder, currency_id, classification_id,
       previous_balance, previous_last_entry_id,
       new_balance, new_last_entry_id, drift, actor_id, created_at
FROM checkpoint_rebuilds
WHERE account_holder = sqlc.arg(account_holder)::bigint
  AND currency_id = sqlc.arg(currency_id)::bigint
  AND classification_id = sqlc.arg(classification_id)::bigint
ORDER BY created_at DESC;

-- name: ListSystemRollupsRaw :many
-- Raw (internal-id) read of system_rollups, for comparing against the
-- entries-based recompute in ReconcileAccountingEquation (M4/I-23:
-- system_rollups must be checked against journal_entries directly, not via
-- balance_checkpoints — AggregateCheckpointsByClassification, which
-- RefreshSystemRollups uses to populate this table, sums checkpoints and so
-- inherits any checkpoint tampering). Distinct from
-- postgres.QueryStore.GetSystemRollups, which resolves to uid/code for public
-- API responses; the reconcile pipeline stays in internal-id space like every
-- other check.
SELECT currency_id, classification_id, total_balance
FROM system_rollups
ORDER BY currency_id, classification_id;

-- name: ReconcileLatestSnapshotDrift :many
-- Entries-based validation for balance_snapshots (M4/I-23): recomputes each
-- balance directly from journal_entries as of the most recent snapshot_date's
-- cutoff (balance_checkpoints never appears here either) and returns only
-- rows whose stored snapshot disagrees with that recomputation. Scoped to the
-- single most recent snapshot_date rather than full history, to bound cost —
-- see docs/plans/2026-08-21-tamper-evident-ledger-design.md M4. When
-- balance_snapshots is empty, latest_date.d is NULL and the final WHERE
-- matches zero rows (nothing to check yet, not a violation).
WITH latest_date AS (
  SELECT MAX(snapshot_date) AS d FROM balance_snapshots
),
recomputed AS (
  SELECT
    je.account_holder,
    je.currency_id,
    je.classification_id,
    COALESCE(SUM(CASE
      WHEN c.normal_side = 'credit' AND je.entry_type = 'credit' THEN je.amount
      WHEN c.normal_side = 'credit' AND je.entry_type = 'debit'  THEN -je.amount
      WHEN je.entry_type = 'debit' THEN je.amount
      ELSE -je.amount
    END), 0)::numeric AS balance
  FROM journal_entries je
  INNER JOIN classifications c ON c.id = je.classification_id
  WHERE je.effective_at < (SELECT d FROM latest_date) + INTERVAL '1 day'
  GROUP BY je.account_holder, je.currency_id, je.classification_id
)
SELECT
  s.account_holder,
  s.currency_id,
  s.classification_id,
  s.snapshot_date,
  s.balance                          AS stored_balance,
  COALESCE(r.balance, 0)::numeric    AS recomputed_balance
FROM balance_snapshots s
LEFT JOIN recomputed r
  ON r.account_holder    = s.account_holder
 AND r.currency_id       = s.currency_id
 AND r.classification_id = s.classification_id
WHERE s.snapshot_date = (SELECT d FROM latest_date)
  AND s.balance <> COALESCE(r.balance, 0)
ORDER BY s.account_holder, s.currency_id, s.classification_id
LIMIT sqlc.arg(page_limit)::int;
