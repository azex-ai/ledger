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
--
-- ⚠️ The two SOLVENCY queries below (GetTotalUserSideBalance,
-- GetSystemSideCustodialBalance) are the exception to the paragraph above:
-- they never reference balance_checkpoints at all. I-49 settled that a
-- checkpoint is an untrusted derived cache -- ledger_app INSERTs into it
-- freely and nothing in it is signed -- and one forged row used to move
-- SolvencyCheck from solvent=false to solvent=true, silencing the library's
-- only unbacked-issuance alarm (w3-review/money-path.md M-2). They now sum
-- journal_entries from the beginning of history, the same trusted basis
-- integrity_checkpoint.sql's RecomputeCheckpointFromEntries uses for I-23.
-- That costs a full scan of the currency's entries per call; solvency is a
-- periodic report, and being cheap in the direction of "no alarm" is not a
-- trade this library makes.

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
-- Returns the sum of all user-side (holder > 0) LIABILITY balances for a
-- currency — what the platform owes users in aggregate — recomputed from
-- journal_entries alone (see the ⚠️ note in this file's header).
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
SELECT COALESCE(SUM(
  ledger_signed_amount(c.normal_side, je.entry_type, je.amount)
), 0)::numeric AS total
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.currency_id = $1
  AND je.account_holder > 0
  AND c.balance_role NOT IN ('', 'memo');

-- name: GetSystemSideCustodialBalance :one
-- Returns the sum of system-side (holder < 0) balances for every
-- classification in the caller's custodial scope, for the given currency,
-- recomputed from journal_entries alone (see the ⚠️ note in this file's
-- header).
--
-- The scope is a parameter, not the string literal 'custodial' it used to be.
-- Two things broke because it was hardcoded (2026-09-02 audit A-N3 / A-M6):
-- a deployment that named its custody classification anything else silently
-- got Custodial = 0 and permanent insolvency, and every deployment running
-- the FX presets read solvent=false forever on each currency it bought,
-- because the asset backing a bought currency sits in `settlement`, not in
-- `custodial` (presets/fx.go). PlatformBalanceStore defaults the scope to
-- {custodial, settlement} and refuses to report at all when no classification
-- matches, and refuses a scope naming a classification that is not a
-- custodied asset -- see ListClassificationScopeAttributes below.
SELECT COALESCE(SUM(
  ledger_signed_amount(c.normal_side, je.entry_type, je.amount)
), 0)::numeric AS total
FROM journal_entries je
INNER JOIN classifications c ON c.id = je.classification_id
WHERE je.currency_id    = sqlc.arg(currency_id)::bigint
  AND je.account_holder < 0
  AND c.code            = ANY(sqlc.arg(custodial_codes)::text[]);

-- name: ListClassificationScopeAttributes :many
-- The attributes that decide whether a classification may stand in the
-- custodial (asset) side of the solvency report, for each of the given codes.
-- A code that exists comes back exactly once; a code that does not exist does
-- not come back at all, which is how the caller names the missing ones.
--
-- This replaced a COUNT(*). The count answered "did ANY of these codes exist",
-- which caught a scope that matched nothing and passed a scope with one typo
-- in it: WithCustodialClassCodes("custodial", "setlement") matched one, and
-- the entire settlement position (FX inventory, transit) went missing from
-- the asset side with no error (w3-review/money-path.md m-1). Multi-code
-- scopes are what §7.3 introduced, so "matched some" is the case that needed
-- catching.
--
-- is_system and balance_role are here for m-2: which classifications count as
-- custodied assets was reasoning that existed only in
-- DefaultCustodialClassCodes' doc comment, so one line of consumer config
-- could put an unbacked or holder-facing classification on the asset side and
-- make the shortfall this report exists to expose disappear. §7.3 moved that
-- judgement from a hardcoded code to a classification property; the caller
-- enforces the property.
SELECT code, is_system, balance_role
FROM classifications
WHERE code = ANY(sqlc.arg(codes)::text[]);
