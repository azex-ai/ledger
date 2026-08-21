-- 052_grant_coverage_gap.up.sql
--
-- P1-follow: close a GRANT coverage gap between 042 and tables/columns
-- whose ACL should reflect their append-only status.
--
-- Part 1 -- missing grants. 042's GRANT loop only enumerates tables that
-- existed AT THE TIME 042 ran, and deliberately leaves `ALTER DEFAULT
-- PRIVILEGES` benefiting only `ledger_owner` -- every future migration that
-- adds a table is required to GRANT ledger_app/ledger_ro on it explicitly
-- (042's header, contracts.md §9 point 3). `reconcile_scan_cursors` (043)
-- and `checkpoint_rebuilds` (050) were both written before 042 merged and
-- never got that grant -- `grep -c ledger_app` is 0 in both files. Both are
-- already merged and migrations are never modified after merge
-- (deployment.md), so this is a new, additive migration rather than a fix
-- to 043/050.
--
-- Consequence if left unfixed (not yet visible -- 049 has not cut
-- DATABASE_URL over to ledger_app in any environment): once it does,
-- reconcile's persisted resume cursor (043, C4b) and RebuildCheckpoint's
-- audit trail (050) would fail to read/write with permission denied. The
-- existing role tests (roles_test.go) never caught this because they only
-- exercise journals/journal_entries/currencies.
--
-- Part 2 -- ACL/trigger consistency. Team Lead review of this migration
-- (before it merged) flagged that granting `checkpoint_rebuilds` UPDATE was
-- inconsistent: it carries the same unconditional `ledger_block_mutation()`
-- append-only guard journal_entries does (050), so the ACL should say the
-- same thing the trigger does, not rely on the trigger alone as the only
-- layer -- two independent defenses against two independent bypass paths
-- (`DROP TRIGGER` needs ownership; `UPDATE` only needs a GRANT) is the
-- whole point of defense-in-depth, and 042's original combined-migration
-- incident (see 042's header) is a direct example of one layer failing
-- silently while a test only exercised the other. `period_closes` (026)
-- carries the identical guard (045 A5) but was granted UPDATE by 042
-- (period_closes existed before 042 ran) and never revoked -- the same
-- inconsistency, just introduced two migrations earlier. Revoked here.
--
-- The rule this migration satisfies, made structural rather than a fixed
-- table list going forward, is: a table with a BEFORE UPDATE trigger
-- executing `ledger_block_mutation()` gets ledger_app SELECT/INSERT only;
-- every other table gets SELECT/INSERT/UPDATE. See
-- postgres.TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants,
-- which derives that set from information_schema.triggers (not a hardcoded
-- name list) and will fail the moment any future migration (047's
-- ledger_attestations/entry_attestations, 051's auth_status column, or a
-- new append-only-guarded table) lands with an ACL that disagrees with its
-- own trigger.

GRANT SELECT, INSERT, UPDATE ON public.reconcile_scan_cursors TO ledger_app;
GRANT SELECT, INSERT ON public.checkpoint_rebuilds TO ledger_app;
GRANT USAGE, SELECT ON public.checkpoint_rebuilds_id_seq TO ledger_app;

GRANT SELECT ON public.reconcile_scan_cursors TO ledger_ro;
GRANT SELECT ON public.checkpoint_rebuilds TO ledger_ro;
GRANT SELECT ON public.checkpoint_rebuilds_id_seq TO ledger_ro;

REVOKE UPDATE ON public.period_closes FROM ledger_app;
