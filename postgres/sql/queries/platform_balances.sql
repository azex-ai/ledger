-- platform_balances.sql
--
-- Queries for reading structured platform-wide balance breakdowns.
-- These read from balance_checkpoints (not system_rollups) because we need
-- to split user-side (holder > 0) from system-side (holder < 0) per
-- classification — system_rollups aggregates all holders together.
--
-- These queries are O(C) where C is the number of distinct classifications,
-- not O(K) where K is the number of users. They are safe for frequent use.

-- name: GetPlatformBalancesByHolder :many
-- Returns the total checkpoint balance per (classification_id, holder_sign, classification_code)
-- for a given currency. holder_sign is 'user' when account_holder > 0, 'system' otherwise.
SELECT
  c.code                             AS classification_code,
  CASE WHEN bc.account_holder > 0 THEN 'user' ELSE 'system' END AS holder_side,
  COALESCE(SUM(bc.balance), 0)::numeric  AS total_balance
FROM balance_checkpoints bc
INNER JOIN classifications c ON c.id = bc.classification_id
WHERE bc.currency_id = $1
GROUP BY c.code, CASE WHEN bc.account_holder > 0 THEN 'user' ELSE 'system' END
ORDER BY c.code, holder_side;

-- name: GetTotalUserSideBalance :one
-- Returns the sum of all user-side (holder > 0) checkpoint balances for a currency.
-- This is the total liability — what the platform owes users in aggregate.
SELECT COALESCE(SUM(balance), 0)::numeric AS total
FROM balance_checkpoints
WHERE currency_id = $1
  AND account_holder > 0;

-- name: GetSystemSideCustodialBalance :one
-- Returns the sum of system-side (holder < 0) balances for the "custodial"
-- classification for the given currency.
SELECT COALESCE(SUM(bc.balance), 0)::numeric AS total
FROM balance_checkpoints bc
INNER JOIN classifications c ON c.id = bc.classification_id
WHERE bc.currency_id = $1
  AND bc.account_holder < 0
  AND c.code = 'custodial';
