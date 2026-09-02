-- Deep audit 2026-09-02, D-m1 / D-m7 / D-m8. Three narrowings of what a
-- leaked ledger_app credential can reach, plus the ownership sweep 019
-- defined and deliberately did not call.

------------------------------------------------------------
-- 1. D-m1: ledger_rebalance_default_partition accepts any date range.
--
-- 013 hardened this function's sibling, ledger_create_monthly_partition,
-- because "EXECUTE on the function is itself a ledger_app-reachable
-- capability" and its name argument reaches DDL through format(%I). The exact
-- same sentence is true of this one's date arguments, and 013 stopped at the
-- one function it was looking at.
--
-- Measured, as ledger_app, on a clean install of 001-015:
--
--     SELECT array_length(
--         ledger_rebalance_default_partition('2020-01-01','2021-12-01'), 1);
--     -- 24
--     -- pg_class then held 60 new relations under journal_entries_y2020%%
--     -- and journal_entries_y2021%% (12 partitions x table + pkey + 3 indexes)
--
-- Every month in the range is a partition table plus four dependent
-- relations, ledger_app cannot DROP any of them (they are owner-gated), and
-- each call takes ACCESS EXCLUSIVE on journal_entries for the DETACH/ATTACH
-- dance -- so a wide range is a one-way availability defect needing a DBA,
-- and a loop of them is a write-path outage. 013 used those exact words about
-- the sibling.
--
-- The legitimate caller (postgres/partition_store.go rebalanceDefault) always
-- passes a month-aligned pair spanning PartitionConfig.MonthsAhead, so the
-- constraints below cost it nothing -- which is the same premise 013 argued
-- from. The cap is on the caller's arguments only: the function then widens
-- the range to cover rows actually sitting in the default partition, and that
-- widening must stay uncapped or a lapsed horizon becomes unrecoverable.
------------------------------------------------------------
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
    -- Ten years of monthly partitions. Far past any horizon
    -- PartitionConfig.MonthsAhead is meant to express (the shipped default is
    -- 3), and still a bounded, recoverable amount of DDL.
    max_months CONSTANT integer := 120;
BEGIN
    IF date_trunc('month', p_first)::date <> p_first OR date_trunc('month', p_last)::date <> p_last THEN
        RAISE EXCEPTION 'ledger: partition rebalance range must be month-aligned, got % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_last < p_first THEN
        RAISE EXCEPTION 'ledger: partition rebalance range ends before it starts: % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF (EXTRACT(YEAR FROM age(p_last, p_first)) * 12 + EXTRACT(MONTH FROM age(p_last, p_first))) > max_months THEN
        RAISE EXCEPTION 'ledger: partition rebalance range % .. % spans more than % months', p_first, p_last, max_months
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

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

------------------------------------------------------------
-- 2. D-m8: nothing has ever looked at function EXECUTE privileges.
--
-- postgres/grant_coverage_test.go is the strictest gate in this schema for
-- tables and sequences -- an unclassified new table fails it outright -- and
-- it does not read pg_proc at all. That blind spot is why 007 could hand
-- ledger_app EXECUTE on two SECURITY DEFINER functions owned by the bootstrap
-- credential and nothing noticed for two audit rounds.
--
-- Closing it needs one thing done here first. A newly created function is
-- EXECUTE-able by PUBLIC unless the migration says otherwise, and only three
-- migrations ever did (007, 009, 013). So today ledger_app effectively holds
-- EXECUTE on every guard and audit function in the schema, by default, and a
-- gate asserting "ledger_app's EXECUTE set is exactly this whitelist" would
-- be red on all of them. Revoking PUBLIC across the board is what makes the
-- whitelist a statement about intent instead of about which migration
-- remembered.
--
-- Safe for the trigger functions: Postgres checks EXECUTE on a trigger
-- function when the trigger is CREATEd, not when it fires. The guards keep
-- working for a caller with no EXECUTE at all -- which is the point, since
-- nothing should be calling ledger_block_mutation() directly.
--
-- Derived from the catalogue, then the exceptions granted back by name. The
-- five ledger_app needs are the two partition entry points (007/013) and the
-- three sign helpers 009 introduced, which appear inside ordinary queries
-- ledger_app runs (SUM(ledger_signed_amount(...)) in balances, trends,
-- reconcile and the holder surface); ledger_ro runs the same read queries and
-- needs the three sign helpers for them.
------------------------------------------------------------
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT p.oid::regprocedure AS sig
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p')
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', r.sig);
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM ledger_app, ledger_ro', r.sig);
    END LOOP;
END $$;

GRANT EXECUTE ON FUNCTION ledger_create_monthly_partition(text, date, date) TO ledger_app;
GRANT EXECUTE ON FUNCTION ledger_rebalance_default_partition(date, date) TO ledger_app;
GRANT EXECUTE ON FUNCTION ledger_reject_unknown_normal_side(text) TO ledger_app, ledger_ro;
GRANT EXECUTE ON FUNCTION ledger_signed_amount(text, text, numeric) TO ledger_app, ledger_ro;
GRANT EXECUTE ON FUNCTION ledger_signed_delta(text, numeric, numeric) TO ledger_app, ledger_ro;

------------------------------------------------------------
-- 3. D-m7: deposits and withdrawals are dead tables with live grants.
--
-- Both predate the unified booking model. Their ten generated sqlc methods
-- have no caller outside postgres/sqlcgen itself, no query file references
-- them after this migration's companion commit deletes
-- postgres/sql/queries/{deposits,withdrawals}.sql, and no trigger guards
-- either table -- yet ledger_app held SELECT/INSERT/UPDATE on both.
--
-- Nothing reads them, so tampering with them changes no behavior today. The
-- risk is the shape: they are the two tables in this schema whose names most
-- look like money, they will mislead whoever reads the schema during an
-- incident, and the day someone wires one back into a read path it arrives
-- pre-granted, unguarded and unaudited.
--
-- deployment.md's contract stage is a DROP, and this is the expand/migrate
-- half of it: revoke the writes, delete the queries, keep the rows. The DROP
-- is deferred so that a deployment which has real history in these tables
-- gets a release in which the tables still exist and nothing writes to them,
-- before the release that removes them (audit TODO: Wave 3).
------------------------------------------------------------
REVOKE INSERT, UPDATE ON public.deposits FROM ledger_app;
REVOKE INSERT, UPDATE ON public.withdrawals FROM ledger_app;

------------------------------------------------------------
-- 4. D-M1: the sweep 019 defines, run once, last.
--
-- Everything above modifies objects 001-018 created, which the migration
-- credential can only do while it still owns them. See 019's header.
------------------------------------------------------------
SELECT ledger_resweep_ownership();
