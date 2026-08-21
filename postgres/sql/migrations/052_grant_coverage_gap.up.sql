-- 052_grant_coverage_gap.up.sql
--
-- P1-follow: close a GRANT coverage gap between 042 and two tables created
-- by migrations written and merged before 042 landed.
--
-- 042's GRANT loop only enumerates tables that existed AT THE TIME 042 ran,
-- and deliberately leaves `ALTER DEFAULT PRIVILEGES` benefiting only
-- `ledger_owner` -- every future migration that adds a table is required to
-- GRANT ledger_app/ledger_ro on it explicitly (042's header, contracts.md
-- §9 point 3). `reconcile_scan_cursors` (043) and `checkpoint_rebuilds`
-- (050) were both written before 042 merged and never got that grant --
-- `grep -c ledger_app` is 0 in both files. Both are already merged and
-- migrations are never modified after merge (deployment.md), so this is a
-- new, additive migration rather than a fix to 043/050.
--
-- Consequence if left unfixed (not yet visible -- 049 has not cut
-- DATABASE_URL over to ledger_app in any environment): once it does,
-- reconcile's persisted resume cursor (043, C4b) and RebuildCheckpoint's
-- audit trail (050) would fail to read/write with permission denied. The
-- existing role tests (roles_test.go) never caught this because they only
-- exercise journals/journal_entries/currencies.
--
-- Grant shape matches 042's own policy exactly: SELECT/INSERT/UPDATE to
-- ledger_app, SELECT to ledger_ro, plus USAGE+SELECT (ledger_app) / SELECT
-- (ledger_ro) on checkpoint_rebuilds_id_seq (reconcile_scan_cursors has no
-- serial column, so no matching sequence).
--
-- Note: checkpoint_rebuilds already has its own BEFORE UPDATE/DELETE
-- append-only triggers (050, same ledger_block_mutation() journal_entries
-- uses) -- granting UPDATE at the ACL layer does not weaken that; the
-- trigger rejects it unconditionally regardless of ACL, matching 042's own
-- "042's GRANT is not enough" observation, just satisfied by a different
-- migration this time.
--
-- This migration is deliberately narrow (two named tables) -- the pin that
-- actually prevents recurrence is structural, not this file: see
-- postgres.TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants,
-- which enumerates every table in `public` and will fail the moment any
-- future migration (047's ledger_attestations/entry_attestations, 051's
-- auth_status column, or anything after) lands without its own GRANT.

GRANT SELECT, INSERT, UPDATE ON public.reconcile_scan_cursors TO ledger_app;
GRANT SELECT, INSERT, UPDATE ON public.checkpoint_rebuilds TO ledger_app;
GRANT USAGE, SELECT ON public.checkpoint_rebuilds_id_seq TO ledger_app;

GRANT SELECT ON public.reconcile_scan_cursors TO ledger_ro;
GRANT SELECT ON public.checkpoint_rebuilds TO ledger_ro;
GRANT SELECT ON public.checkpoint_rebuilds_id_seq TO ledger_ro;
