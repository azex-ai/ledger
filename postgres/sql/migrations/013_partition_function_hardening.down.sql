-- Reverts m-1 (search_path pg_temp) and m-2 (name/range consistency check)
-- to migration 007's original two function bodies.

CREATE OR REPLACE FUNCTION ledger_create_monthly_partition(
    p_name text, p_from date, p_to date
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    IF p_name !~ '^journal_entries_y[0-9]{4}m[0-9]{2}$' THEN
        RAISE EXCEPTION 'ledger: invalid monthly partition name %', p_name
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF to_regclass('public.' || p_name) IS NOT NULL THEN
        RETURN false;
    END IF;
    EXECUTE format(
        'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
        p_name, p_from, p_to
    );
    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION ledger_create_monthly_partition(text, date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_create_monthly_partition(text, date, date) TO ledger_app;

ALTER FUNCTION ledger_rebalance_default_partition(date, date)
    SET search_path = public;
