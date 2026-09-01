-- Restore 001_baseline's table-level grant for webhook_subscribers.
REVOKE UPDATE (last_status_code, last_error, last_attempt_at)
    ON public.webhook_subscribers FROM ledger_app;
GRANT INSERT, UPDATE ON public.webhook_subscribers TO ledger_app;
