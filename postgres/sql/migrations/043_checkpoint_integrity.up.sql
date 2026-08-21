-- 043_checkpoint_integrity.up.sql
--
-- P2 of the integrity-hardening wave (checkpoint un-trust + resume cursor).
-- See docs/plans/2026-08-21-tamper-evident-ledger-design.md §4 and
-- docs/plans/2026-08-21-integrity-hardening-contracts.md.
--
-- This migration adds exactly one table: a persisted resume cursor for
-- reconcile's fleet-wide scans (C4b). RecomputeBalance and RebuildCheckpoint
-- (service/postgres code in this same phase) need no schema change -- they
-- read/overwrite the existing balance_checkpoints table using new queries
-- only (postgres/sql/queries/integrity_checkpoint.sql).

------------------------------------------------------------
-- 1. Persisted resume cursor for check #2's fleet-wide checkpoint scan.
--
--    Before this table, the keyset cursor lived only in FullReconciliationConfig
--    call-scoped memory: every scheduled run restarted from
--    (MinInt64, MinInt64). With Check2ScanLimit defaulting to 5000, any fleet
--    larger than that limit had its tail permanently unscanned -- every run
--    re-verified the same prefix and never reached the rest (see
--    docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md, "未解决" section).
--
--    One row per check name (currently just "checkpoint_balance", but keyed
--    by name so a future fleet-wide check can reuse the same mechanism
--    without a schema change). after_holder/after_currency default to the
--    same MinInt64 sentinel the in-memory cursor used, matching the fix in
--    docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md (B1): a (0, 0)
--    start would exclude every negative (system) holder.
--
--    lap_dirty carries "did any segment of the CURRENT lap already find a
--    violation" across resumptions. Without it, a lap that spans N runs
--    could report Passed=true on the run that happens to complete the lap
--    even though an earlier run within that same lap found real drift --
--    exactly the "looks green when it isn't" defect P0 fixed for the
--    single-run case (docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md).
--    Reset to false only when a lap completes (the cursor also resets to the
--    start sentinel at that point).
------------------------------------------------------------
CREATE TABLE reconcile_scan_cursors (
    check_name     TEXT NOT NULL PRIMARY KEY,
    after_holder   BIGINT NOT NULL DEFAULT -9223372036854775808,
    after_currency BIGINT NOT NULL DEFAULT -9223372036854775808,
    lap_dirty      BOOLEAN NOT NULL DEFAULT false,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
