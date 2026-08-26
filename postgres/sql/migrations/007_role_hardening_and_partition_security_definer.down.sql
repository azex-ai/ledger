REVOKE ALL ON FUNCTION ledger_rebalance_default_partition(date, date) FROM ledger_app;
DROP FUNCTION IF EXISTS ledger_rebalance_default_partition(date, date);

REVOKE ALL ON FUNCTION ledger_create_monthly_partition(text, date, date) FROM ledger_app;
DROP FUNCTION IF EXISTS ledger_create_monthly_partition(text, date, date);

REVOKE SELECT (
    id, name, url, filter_class, filter_to_status, is_active,
    created_at, last_status_code, last_error, last_attempt_at
) ON public.webhook_subscribers FROM ledger_ro;
GRANT SELECT ON public.webhook_subscribers TO ledger_ro;

-- Role attribute hardening is not reversed: re-widening a role's privileges
-- on rollback is never the safe direction, and nothing legitimate depended
-- on SUPERUSER/CREATEDB/CREATEROLE/REPLICATION/BYPASSRLS being available.
