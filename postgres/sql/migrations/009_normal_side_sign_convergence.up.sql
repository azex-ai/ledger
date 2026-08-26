-- Collapse the 10 independent SQL sign expressions that interpret
-- classifications.normal_side against journal_entries.entry_type into two
-- functions, mirroring the Go-side collapse into core.Sign / core.Delta
-- (docs/audits/2026-08-25-financial-engineering/financial-correctness.md,
-- "同一个符号语义有 17 处独立实现"; contract
-- docs/plans/2026-08-26-audit-remediation-contracts.md §9 W3-sign).
--
-- Three shapes existed before this migration, all encoding the same rule
-- ("an entry increases a normal_side-normal balance when its entry_type
-- equals normal_side, decreases it otherwise") with different fates for a
-- normal_side outside {debit, credit}:
--
--   1. 4-way CASE, ELSE 0 (checkpoints.sql's ListComputedBalancesForHolders,
--      integrity_checkpoint.sql's RecomputeCheckpointFromEntries) --
--      silently excluded the entry from the balance. This is the one that
--      loses money: the entry is not rejected, not logged, not counted at
--      either sign -- it just vanishes from the SUM.
--   2. 3-way CASE with an implicit debit-normal fallback
--      (checkpoints.sql's ListBalancesAt, integrity_checkpoint.sql's
--      ReconcileLatestSnapshotDrift), an OR-combined two-way CASE with the
--      same fallback (platform_balances.sql's three LATERAL subqueries),
--      and a bare string-equality expression with the same fallback
--      (holder.sql) -- these mis-sign an unrecognized normal_side instead
--      of dropping it, but still never raise.
--   3. MIN(c.normal_side) = 'debit' branching on a pre-aggregated
--      (debit_sum, credit_sum) pair (reconcile.sql's
--      ReconcileNonNegativeBalances HAVING clause) -- same fallback
--      direction as (2), one level removed from the per-row amount.
--
-- Both classifications.normal_side and journal_entries.entry_type carry a
-- `CHECK (... IN ('debit', 'credit'))` constraint since 001_baseline
-- (:169, :220, :331), and normal_side is immutable once set
-- (classifications_normal_side_immutable trigger, 001_baseline :1131) --
-- so every one of the three fallback shapes above is dead code today: no
-- row this codebase can produce ever reaches the ELSE/fallback branch. That
-- CHECK constraint is what makes ledger_signed_amount's ELSE branch safe to
-- implement as a hard failure (RAISE EXCEPTION) rather than another silent
-- default: the branch is provably unreachable for any row already in the
-- table, so raising there can never turn a previously-succeeding query into
-- a failing one. It exists as a fail-closed backstop against a future
-- write path that skips the CHECK (a raw ledger_owner INSERT, a restore
-- from a backup taken before the CHECK existed), not as a hot-path
-- concern -- see the inlining discussion below for why it costs nothing on
-- the path that stays in bounds.
--
-- ####  Two functions, two shapes, one rule  ####
--
-- ledger_signed_amount(normal_side, entry_type, amount) is the per-entry
-- form: it mirrors core.SignedAmount exactly and replaces every one of the
-- CASE/OR/string-equality expressions above. LANGUAGE sql + IMMUTABLE +
-- PARALLEL SAFE with a single-statement body is Postgres's inlining
-- contract (https://www.postgresql.org/docs/current/xfunc-sql.html#XFUNC-SQL-FUNCTIONS-AS-TABLE-SOURCES,
-- "SQL functions ... are normally inlined"): the planner substitutes the
-- CASE expression directly into the calling query's plan, so on the hot
-- read paths (ListComputedBalancesForHolders backs GetBalances /
-- BatchGetBalances / GetBalanceBreakdown) this compiles down to the same
-- CASE it replaces, not a per-row function-call. The ELSE branch calls
-- ledger_reject_unknown_normal_side, a separate LANGUAGE plpgsql function
-- (RAISE is not available in a LANGUAGE sql function body) -- CASE
-- branches that are not selected are never evaluated, so this call is
-- never actually made for any real row and does not defeat inlining of the
-- surrounding SELECT.
--
-- ⚠ Deliberately NOT declared STRICT, even though several callers
-- (RecomputeCheckpointFromEntries, ListComputedBalancesForHolders) LEFT
-- JOIN journal_entries against a classification that may have zero
-- matching rows on purpose -- that is how "this classification exists but
-- has no entries yet" produces balance=0 instead of no row at all, and
-- Postgres represents that absent match by passing entry_type/amount as
-- NULL for the (still-produced) joined row. The leading
-- `WHEN p_entry_type IS NULL THEN NULL` branch handles that case
-- explicitly instead of via a STRICT qualifier: SUM() ignores a NULL
-- input the same way it always has, so COALESCE(SUM(...), 0) one level up
-- still turns "this row contributed nothing" back into 0, matching the
-- ELSE 0 shape this function replaces for that one case. Marking the
-- function STRICT instead was tried and measured first -- empirically, a
-- STRICT LANGUAGE SQL function is never inlined by the planner even when
-- its body would otherwise qualify (confirmed via EXPLAIN VERBOSE: dropping
-- STRICT is what makes ledger_signed_amount(...) disappear from the plan
-- and get replaced by its CASE body; with STRICT the function call itself
-- remained in the plan). The explicit NULL guard gets the identical
-- correctness with the CASE staying inlinable -- see
-- BenchmarkListComputedBalancesForHolders in postgres/benchmarks_test.go
-- for the measured cost of getting this wrong. A non-NULL,
-- non-{debit,credit} p_normal_side or p_entry_type still falls through
-- every WHEN to the ELSE below and still raises -- the NULL guard only
-- catches the "no matching entry" LEFT JOIN case, not a genuinely
-- unrecognized symbol.
--
-- ledger_signed_delta(normal_side, debit_sum, credit_sum) is the
-- pre-aggregated form (mirrors core.Delta, itself defined as
-- SignedAmount(debit) + SignedAmount(credit) -- see core/account_policy.go)
-- for the one query that branches on already-summed totals instead of
-- per-row amounts (reconcile.sql's ReconcileNonNegativeBalances). Every
-- caller already passes COALESCE(SUM(...), 0)-wrapped, never-NULL sums and
-- a MIN(c.normal_side) drawn from a non-empty GROUP BY, so there is no
-- equivalent "absent row" case to guard here.
--
-- See docs/INVARIANTS.md I-43.

CREATE OR REPLACE FUNCTION ledger_reject_unknown_normal_side(p_normal_side text)
RETURNS numeric
LANGUAGE plpgsql
IMMUTABLE
AS $$
BEGIN
    RAISE EXCEPTION 'ledger: unknown normal_side %, expected debit or credit', p_normal_side
        USING ERRCODE = 'invalid_parameter_value';
END;
$$;

REVOKE ALL ON FUNCTION ledger_reject_unknown_normal_side(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_reject_unknown_normal_side(text) TO ledger_app, ledger_ro;

CREATE OR REPLACE FUNCTION ledger_signed_amount(p_normal_side text, p_entry_type text, p_amount numeric)
RETURNS numeric
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT CASE
        WHEN p_entry_type IS NULL THEN NULL
        WHEN p_normal_side = 'debit'  AND p_entry_type = 'debit'  THEN p_amount
        WHEN p_normal_side = 'debit'  AND p_entry_type = 'credit' THEN -p_amount
        WHEN p_normal_side = 'credit' AND p_entry_type = 'credit' THEN p_amount
        WHEN p_normal_side = 'credit' AND p_entry_type = 'debit'  THEN -p_amount
        ELSE ledger_reject_unknown_normal_side(p_normal_side)
    END;
$$;

REVOKE ALL ON FUNCTION ledger_signed_amount(text, text, numeric) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_signed_amount(text, text, numeric) TO ledger_app, ledger_ro;

CREATE OR REPLACE FUNCTION ledger_signed_delta(p_normal_side text, p_debit_sum numeric, p_credit_sum numeric)
RETURNS numeric
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT ledger_signed_amount(p_normal_side, 'debit', p_debit_sum)
         + ledger_signed_amount(p_normal_side, 'credit', p_credit_sum);
$$;

REVOKE ALL ON FUNCTION ledger_signed_delta(text, numeric, numeric) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_signed_delta(text, numeric, numeric) TO ledger_app, ledger_ro;
