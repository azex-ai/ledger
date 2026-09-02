-- ⚠️ Must run from a superuser connection, or with `SET ROLE ledger_owner`
-- around the statements: 021's last act transferred every object in `public`
-- to ledger_owner, and the migration credential holds SET but not INHERIT on
-- that role. See 019's down for the shape. Ownership itself is deliberately
-- not reversed -- 019's down explains why that direction is the dangerous one.

-- 3. Restore the dead tables' write grants (migration 001's derived ACL loop
--    classified both as ordinary tables).
GRANT INSERT, UPDATE ON public.deposits TO ledger_app;
GRANT INSERT, UPDATE ON public.withdrawals TO ledger_app;

-- 2. Restore the PUBLIC EXECUTE default every function except 007/009/013's
--    five carried before this migration.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT p.oid::regprocedure AS sig, p.proname
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p')
          AND p.proname NOT IN (
              'ledger_create_monthly_partition',
              'ledger_rebalance_default_partition',
              'ledger_reject_unknown_normal_side',
              'ledger_signed_amount',
              'ledger_signed_delta',
              'ledger_resweep_ownership'
          )
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO PUBLIC', r.sig);
    END LOOP;
END $$;

-- 1. Drop ledger_rebalance_default_partition's argument validation, restoring
--    migration 007's body verbatim.
CREATE OR REPLACE FUNCTION ledger_rebalance_default_partition(
    p_first date, p_last date
) RETURNS text[]
LANGUAGE plpgsql
SECURITY DEFINER
-- public, pg_temp, not bare public: migration 013 (m-1) added pg_temp because an
-- unqualified search_path implicitly searches pg_temp first, which a caller can
-- populate. CREATE OR REPLACE rewrites proconfig wholesale, so restating it
-- here is what keeps 013's fix from being silently undone.
SET search_path = public, pg_temp
AS $$
DECLARE
    created    text[] := '{}';
    v_min      timestamptz;
    v_max      timestamptz;
    v_has_rows boolean;
    v_month    date;
    v_name     text;
BEGIN
    ALTER TABLE journal_entries DETACH PARTITION journal_entries_default;

    SELECT min(created_at), max(created_at) INTO v_min, v_max FROM journal_entries_default;
    v_has_rows := v_min IS NOT NULL;

    IF v_has_rows THEN
        IF date_trunc('month', v_min)::date < p_first THEN
            p_first := date_trunc('month', v_min)::date;
        END IF;
        IF date_trunc('month', v_max)::date > p_last THEN
            p_last := date_trunc('month', v_max)::date;
        END IF;
    END IF;

    v_month := p_first;
    WHILE v_month <= p_last LOOP
        v_name := format('journal_entries_y%sm%s', to_char(v_month, 'YYYY'), to_char(v_month, 'MM'));
        IF to_regclass('public.' || v_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
                v_name, v_month, (v_month + INTERVAL '1 month')::date
            );
        END IF;
        created := array_append(created, v_name);
        v_month := (v_month + INTERVAL '1 month')::date;
    END LOOP;

    IF v_has_rows THEN
        INSERT INTO journal_entries SELECT * FROM journal_entries_default;
        TRUNCATE journal_entries_default;
    END IF;

    ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT;

    RETURN created;
END;
$$;

REVOKE ALL ON FUNCTION ledger_rebalance_default_partition(date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_rebalance_default_partition(date, date) TO ledger_app;
