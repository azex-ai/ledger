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
    CASE
      WHEN (c.normal_side = 'debit'  AND je.entry_type = 'debit')
        OR (c.normal_side = 'credit' AND je.entry_type = 'credit')
      THEN je.amount
      ELSE -je.amount
    END
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
-- Returns the realtime sum of all user-side (holder > 0) balances for a currency.
-- This is the total liability — what the platform owes users in aggregate.
WITH active AS (
  SELECT DISTINCT account_holder, classification_id
  FROM journal_entries
  WHERE currency_id = $1
    AND account_holder > 0
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
    CASE
      WHEN (c.normal_side = 'debit'  AND je.entry_type = 'debit')
        OR (c.normal_side = 'credit' AND je.entry_type = 'credit')
      THEN je.amount
      ELSE -je.amount
    END
  ), 0)::numeric AS delta
  FROM journal_entries je
  WHERE je.account_holder    = a.account_holder
    AND je.currency_id       = $1
    AND je.classification_id = a.classification_id
    AND je.id                > COALESCE(bc.last_entry_id, 0)
) d ON TRUE;

-- name: GetSystemSideCustodialBalance :one
-- Returns the realtime sum of system-side (holder < 0) balances for the
-- "custodial" classification for the given currency.
WITH active AS (
  SELECT DISTINCT je.account_holder
  FROM journal_entries je
  INNER JOIN classifications c ON c.id = je.classification_id
  WHERE je.currency_id      = $1
    AND je.account_holder < 0
    AND c.code              = 'custodial'
)
SELECT COALESCE(SUM(COALESCE(bc.balance, 0) + COALESCE(d.delta, 0)), 0)::numeric AS total
FROM active a
INNER JOIN classifications c ON c.code = 'custodial'
LEFT JOIN balance_checkpoints bc
       ON bc.account_holder    = a.account_holder
      AND bc.currency_id       = $1
      AND bc.classification_id = c.id
LEFT JOIN LATERAL (
  SELECT COALESCE(SUM(
    CASE
      WHEN (c.normal_side = 'debit'  AND je.entry_type = 'debit')
        OR (c.normal_side = 'credit' AND je.entry_type = 'credit')
      THEN je.amount
      ELSE -je.amount
    END
  ), 0)::numeric AS delta
  FROM journal_entries je
  WHERE je.account_holder    = a.account_holder
    AND je.currency_id       = $1
    AND je.classification_id = c.id
    AND je.id                > COALESCE(bc.last_entry_id, 0)
) d ON TRUE;
