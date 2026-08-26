DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_partition_tree('journal_entries'::regclass) pt
        JOIN pg_class c ON c.oid = pt.relid
    LOOP
        EXECUTE format(
            'REVOKE INSERT (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at) ON public.%I FROM ledger_app',
            r.relname);
        EXECUTE format('GRANT INSERT ON public.%I TO ledger_app', r.relname);
    END LOOP;
END $$;
