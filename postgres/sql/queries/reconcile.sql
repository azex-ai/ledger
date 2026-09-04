-- Reconciliation queries for the full reconciliation suite
-- (service.FullReconciliationService).
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
--
-- net_balance goes through ledger_signed_amount (docs/INVARIANTS.md I-43), not
-- a bare debit-minus-credit CASE. The zero test is unaffected either way, but
-- the figure is written verbatim into the Finding an operator reads
-- (service/reconcile.go, "net=%s"), and settlement is credit-normal: the bare
-- expression handed them the sign backwards from what GetBalance reports for
-- the same classification (2026-09-02 audit A-N1).
SELECT
  je.currency_id,
  COALESCE(SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)), 0)::numeric AS net_balance
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE c.code = sqlc.arg(classification_code)::text
  AND je.created_at < now() - (sqlc.arg(window_minutes)::int * INTERVAL '1 minute')
GROUP BY je.currency_id
HAVING COALESCE(SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)), 0) != 0
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
HAVING ledger_signed_delta(
  MIN(c.normal_side),
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)
) < 0
ORDER BY je.account_holder, je.classification_id
LIMIT sqlc.arg(page_limit)::int;

-- name: ReconcileRoleLessLiabilities :many
-- M-4 fix (`.local/independent-review-2026-08-26.md`,
-- docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
-- fix-backend-1 batch, board #43): GetTotalUserSideBalance (I-37) sums only
-- user-side classifications tagged with a non-empty balance_role. That is
-- correct GIVEN every real liability classification is actually tagged --
-- nothing in ClassificationInput enforced that (closed separately by
-- ClassificationInput.Validate, docs/INVARIANTS.md I-37 addendum).
--
-- This query does NOT filter on normal_side: an earlier version of this fix
-- filtered `c.normal_side = 'credit'`, reasoning "liability-shaped by
-- construction". That reasoning does not hold in THIS library's own
-- convention -- main_wallet, the canonical real liability, is DEBIT-normal
-- (DR increases what the platform owes the holder). balance_role is the
-- ONLY signal that distinguishes a real liability from a legitimate
-- role-less memo/cost account (fee_expense and friends, also debit-normal)
-- -- normal_side cannot do it. Filtering on normal_side therefore missed
-- exactly the shape a consumer would produce by copying main_wallet without
-- also copying its balance_role, which independent review confirmed
-- end-to-end: SolvencyCheck.Liability stayed unchanged (understated) with
-- such a classification carrying a real, nonzero balance.
--
-- False positives on role-less memo accounts (fee_expense) are avoided by
-- BalanceRoleMemo (migration 011), not by a normal_side filter:
-- ClassificationInput.Validate requires every new non-system classification
-- to declare EITHER a spendable role OR 'memo' explicitly, so "balance_role
-- = ''" on a non-system classification means exactly one thing going
-- forward -- nobody tagged it -- and is safe to treat uniformly as
-- suspicious, regardless of normal_side.
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
  AND c.balance_role = ''
  AND NOT c.is_system
GROUP BY je.account_holder, je.currency_id, je.classification_id, c.normal_side
HAVING ledger_signed_delta(
  MIN(c.normal_side),
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)
) != 0
ORDER BY je.account_holder, je.classification_id
LIMIT sqlc.arg(page_limit)::int;

-- name: ReconcileOrphanReservations :many
-- Check #7: reservations whose journal_id references a non-existent journal.
-- Since migration 035 journal_id is a nullable FK (NULL = no journal), so
-- this can only fire if the FK is ever dropped or disabled — kept as
-- defense-in-depth.
SELECT
  r.id,
  r.uid,
  r.account_holder,
  r.currency_id,
  r.status,
  r.journal_id
FROM reservations r
WHERE r.journal_id IS NOT NULL
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

-- name: ReconcileListCheckpointAccountsPage :many
-- Check #2: keyset-paginated list of distinct (account_holder, currency_id)
-- pairs present in balance_checkpoints, used to drive a fleet-wide
-- checkpoint-vs-entries verification (one ReconcileAccount call per pair).
-- Ordered by (account_holder, currency_id) so pagination is stable and
-- resumable from the last-seen pair. Pass (math.MinInt64, math.MinInt64) for
-- the first page, NOT (0, 0): system holders are negative (core.SystemHolder),
-- so a zero cursor makes this predicate skip the entire system side forever.
SELECT DISTINCT account_holder, currency_id
FROM balance_checkpoints
WHERE account_holder > sqlc.arg(after_holder)::bigint
   OR (account_holder = sqlc.arg(after_holder)::bigint AND currency_id > sqlc.arg(after_currency)::bigint)
ORDER BY account_holder, currency_id
LIMIT sqlc.arg(page_limit)::int;

-- name: GetReconcileScanCursor :one
-- Persisted resume cursor for check #2's fleet-wide scan (C4b). Zero rows
-- (no cursor persisted yet, e.g. first run ever) is a normal state, not an
-- error — the adapter maps it to the same (MinInt64, MinInt64, false, 0) start
-- the in-memory cursor always used, NOT (0, 0): system holders are negative
-- (core.SystemHolder), so a zero start would reintroduce the bug fixed in
-- docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md (B1). lap_dirty
-- carries "did an earlier segment of this lap already find a violation" so
-- the check that completes the lap can still report Passed=false. lap_scanned
-- (M-1 fix, migration 010) carries the cumulative count of pairs verified by
-- every run of the current lap so far, independent of the resumed cursor's
-- own position.
SELECT after_holder, after_currency, lap_dirty, lap_scanned
FROM reconcile_scan_cursors
WHERE check_name = sqlc.arg(check_name)::text;

-- name: UpsertReconcileScanCursor :exec
-- Called at the end of every check #2 run: persists the resume point,
-- lap_dirty flag and cumulative lap_scanned when the scan was capped or
-- timed out (partial coverage), or resets all three to their start values
-- ((MinInt64, MinInt64), false, 0) when a full lap completed, so the next
-- run begins a fresh lap rather than replaying the same resume point (or a
-- stale dirty flag/count) forever.
INSERT INTO reconcile_scan_cursors (check_name, after_holder, after_currency, lap_dirty, lap_scanned, updated_at)
VALUES (sqlc.arg(check_name)::text, sqlc.arg(after_holder)::bigint, sqlc.arg(after_currency)::bigint, sqlc.arg(lap_dirty)::boolean, sqlc.arg(lap_scanned)::bigint, now())
ON CONFLICT (check_name)
DO UPDATE SET after_holder = EXCLUDED.after_holder, after_currency = EXCLUDED.after_currency,
              lap_dirty = EXCLUDED.lap_dirty, lap_scanned = EXCLUDED.lap_scanned, updated_at = now();

-- name: ReconcileCountCheckpointAccountPairs :one
-- Check #2 (M-1 fix, migration 010): the total number of distinct
-- (account_holder, currency_id) pairs currently present in
-- balance_checkpoints -- the same population ReconcileListCheckpointAccountsPage
-- paginates over. Used as the independent cross-check a resumed lap's
-- cumulative lap_scanned must reach before "no more rows after this cursor"
-- is trusted as full coverage.
SELECT COUNT(*)::bigint AS total
FROM (SELECT DISTINCT account_holder, currency_id FROM balance_checkpoints) pairs;

-- name: ReconcileUntaggedHolderKindJournalTypes :many
-- M-7 follow-up (Team Lead, 2026-08-27, board #49, docs/INVARIANTS.md I-44):
-- a journal type visible in the holder-facing transaction view (the same
-- population holder.sql's ListHolderTransactionRows draws from -- a
-- journal entry posted for a user holder against a role-bearing
-- classification) but not tagged with a core.HolderTxKind reads as the
-- generic 'other' bucket on the wire. That fallback is safe (never a
-- financial miscalculation, never a raw internal identifier) but
-- INVISIBLE: nothing tells a deployer they forgot to tag a journal type
-- until a user notices their transaction list shows "other" instead of a
-- specific label. This check surfaces it as a Finding -- detection, not
-- prevention; it does not change what `kind` resolves to on the wire, and
-- fixing a Finding here is a JournalTypeStore.SetHolderKind call, not a
-- migration.
SELECT DISTINCT jt.uid, jt.code, jt.name
FROM journal_types jt
JOIN journals j ON j.journal_type_id = jt.id
JOIN journal_entries je ON je.journal_id = j.id
JOIN classifications c ON c.id = je.classification_id
WHERE jt.holder_kind = ''
  AND je.account_holder > 0
  -- Same predicate as holder.sql: a journal type whose only holder-side entries
  -- are memo trackers never surfaces in a user's transaction list, so it does
  -- not need a holder_kind tag and must not be reported as missing one
  -- (2026-09-02 audit A-N4).
  AND c.balance_role NOT IN ('', 'memo')
ORDER BY jt.uid
LIMIT sqlc.arg(page_limit)::int;
