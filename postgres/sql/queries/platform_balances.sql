-- platform_balances.sql
--
-- Real-time queries for platform-wide balance breakdowns.
--
-- Each query computes balance as `checkpoint.balance + delta` where delta is
-- the net amount of journal_entries with `id > checkpoint.last_entry_id` for
-- the same (account_holder, currency, classification) tuple. This mirrors the
-- single-account `GetBalance` model and reflects every committed write
-- immediately, without waiting for the rollup worker.
--
-- The base set of "active" accounts comes from journal_entries (DISTINCT on
-- the prefix of the composite index `(account_holder, currency_id,
-- classification_id, id)` — index-only scan). Checkpoints are LEFT JOIN'd in
-- so brand-new accounts (entries posted but no rollup yet) still show up
-- correctly with checkpoint_balance=0 and last_entry_id=0.
--
-- The LATERAL subquery runs once per active (holder, classification). When
-- the rollup queue is current most subqueries return empty. The composite
-- index keeps each LATERAL fast.
--
-- Each query is a single SQL statement, so PostgreSQL gives it a single
-- snapshot (no phantom reads between checkpoint and entries). Multi-statement
-- callers (e.g. SolvencyCheck) must wrap in REPEATABLE READ themselves.

-- name: GetPlatformBalancesByHolder :many
-- Returns the realtime balance per (classification_code, holder_side) for a currency.
WITH active AS (
  SELECT DISTINCT account_holder, classification_id
  FROM journal_entries
  WHERE currency_id = $1
)
SELECT
  c.code                                                        AS classification_code,
  CASE WHEN a.account_holder > 0 THEN 'user' ELSE 'system' END  AS holder_side,
  COALESCE(SUM(COALESCE(bc.balance, 0) + COALESCE(d.delta, 0)), 0)::numeric AS total_balance
FROM active a
INNER JOIN classifications c ON c.id = a.classification_id
LEFT JOIN balance_checkpoints bc
       ON bc.account_holder    = a.account_holder
      AND bc.currency_id       = $1
      AND bc.classification_id = a.classification_id
LEFT JOIN LATERAL (
  SELECT COALESCE(SUM(
    ledger_signed_amount(c.normal_side, je.entry_type, je.amount)
  ), 0)::numeric AS delta
  FROM journal_entries je
  WHERE je.account_holder    = a.account_holder
    AND je.currency_id       = $1
    AND je.classification_id = a.classification_id
    AND je.id                > COALESCE(bc.last_entry_id, 0)
) d ON TRUE
GROUP BY c.code, CASE WHEN a.account_holder > 0 THEN 'user' ELSE 'system' END
ORDER BY c.code, holder_side;

-- name: GetTotalUserSideBalance :one
-- Returns the realtime sum of all user-side (holder > 0) LIABILITY balances
-- for a currency — what the platform owes users in aggregate.
--
-- "Liability" is scoped to classifications tagged with balance_role
-- available/pending/locked, the same basis GetBalanceBreakdown uses for a
-- holder's spendable-money view (see I-11 in docs/INVARIANTS.md). Both
-- balance_role = '' (untagged) AND balance_role = 'memo' (M-4 fix,
-- migration 011) are excluded: memo classifications (fee_expense and
-- friends) are debit-normal cost/memo accounts booked to the user's holder
-- id for per-user reporting — never part of what the platform owes back.
-- 'memo' has to be excluded explicitly, not folded into "any non-empty role
-- means liability" the way the original two-value role world allowed --
-- summing a memo-tagged classification into the liability figure would
-- reproduce the exact phantom-insolvency bug this query was originally
-- fixed for (docs/audits/2026-08-25-financial-engineering/financial-correctness.md
-- Major #1, "偿付能力把 user-side debit-normal 费用账当成负债"), just via a
-- different route (an explicit tag instead of a missing one).
WITH active AS (
  SELECT DISTINCT je.account_holder, je.classification_id
  FROM journal_entries je
  INNER JOIN classifications c ON c.id = je.classification_id
  WHERE je.currency_id = $1
    AND je.account_holder > 0
    AND c.balance_role NOT IN ('', 'memo')
)
SELECT COALESCE(SUM(COALESCE(bc.balance, 0) + COALESCE(d.delta, 0)), 0)::numeric AS total
FROM active a
INNER JOIN classifications c ON c.id = a.classification_id
LEFT JOIN balance_checkpoints bc
       ON bc.account_holder    = a.account_holder
      AND bc.currency_id       = $1
      AND bc.classification_id = a.classification_id
LEFT JOIN LATERAL (
  SELECT COALESCE(SUM(
    ledger_signed_amount(c.normal_side, je.entry_type, je.amount)
  ), 0)::numeric AS delta
  FROM journal_entries je
  WHERE je.account_holder    = a.account_holder
    AND je.currency_id       = $1
    AND je.classification_id = a.classification_id
    AND je.id                > COALESCE(bc.last_entry_id, 0)
) d ON TRUE;

-- name: GetSystemSideCustodialBalance :one
-- Returns the realtime sum of system-side (holder < 0) balances for every
-- classification in the caller's custodial scope, for the given currency.
--
-- The scope is a parameter, not the string literal 'custodial' it used to be.
-- Two things broke because it was hardcoded (2026-09-02 audit A-N3 / A-M6):
-- a deployment that named its custody classification anything else silently
-- got Custodial = 0 and permanent insolvency, and every deployment running
-- the FX presets read solvent=false forever on each currency it bought,
-- because the asset backing a bought currency sits in `settlement`, not in
-- `custodial` (presets/fx.go). PlatformBalanceStore defaults the scope to
-- {custodial, settlement} and refuses to report at all when no classification
-- matches -- see CountClassificationsWithCodes below.
WITH active AS (
  SELECT DISTINCT je.account_holder, je.classification_id
  FROM journal_entries je
  INNER JOIN classifications c ON c.id = je.classification_id
  WHERE je.currency_id      = sqlc.arg(currency_id)::bigint
    AND je.account_holder   < 0
    AND c.code              = ANY(sqlc.arg(custodial_codes)::text[])
)
SELECT COALESCE(SUM(COALESCE(bc.balance, 0) + COALESCE(d.delta, 0)), 0)::numeric AS total
FROM active a
INNER JOIN classifications c ON c.id = a.classification_id
LEFT JOIN balance_checkpoints bc
       ON bc.account_holder    = a.account_holder
      AND bc.currency_id       = sqlc.arg(currency_id)::bigint
      AND bc.classification_id = a.classification_id
LEFT JOIN LATERAL (
  SELECT COALESCE(SUM(
    ledger_signed_amount(c.normal_side, je.entry_type, je.amount)
  ), 0)::numeric AS delta
  FROM journal_entries je
  WHERE je.account_holder    = a.account_holder
    AND je.currency_id       = sqlc.arg(currency_id)::bigint
    AND je.classification_id = a.classification_id
    AND je.id                > COALESCE(bc.last_entry_id, 0)
) d ON TRUE;

-- name: CountClassificationsWithCodes :one
-- How many of the given classification codes actually exist. The solvency
-- read uses it as a fail-loud check on its own scope: a scope that matches
-- nothing can only produce Custodial = 0, which is indistinguishable from a
-- genuinely empty custody position and reads as total insolvency
-- (working-agreements.md §3 -- a misconfiguration must not look like a
-- measurement).
SELECT COUNT(*)::bigint AS total
FROM classifications
WHERE code = ANY(sqlc.arg(codes)::text[]);
