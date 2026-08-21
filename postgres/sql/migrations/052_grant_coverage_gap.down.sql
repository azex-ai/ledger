-- 052_grant_coverage_gap.down.sql
--
-- Reverses 052_grant_coverage_gap.up.sql.

GRANT UPDATE ON public.period_closes TO ledger_app;

REVOKE SELECT, INSERT, UPDATE ON public.reconcile_scan_cursors FROM ledger_app;
REVOKE SELECT, INSERT ON public.checkpoint_rebuilds FROM ledger_app;
REVOKE USAGE, SELECT ON public.checkpoint_rebuilds_id_seq FROM ledger_app;

REVOKE SELECT ON public.reconcile_scan_cursors FROM ledger_ro;
REVOKE SELECT ON public.checkpoint_rebuilds FROM ledger_ro;
REVOKE SELECT ON public.checkpoint_rebuilds_id_seq FROM ledger_ro;
