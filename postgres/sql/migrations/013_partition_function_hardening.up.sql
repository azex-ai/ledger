-- Two Minor findings from `.local/independent-review-2026-08-26.md`
-- (independent second-pass review of migration 007's two SECURITY DEFINER
-- partition-maintenance functions), grouped here because both are fixes to
-- the same two function bodies and neither touches application tables.

-- ####  m-1: SET search_path = public did not include pg_temp  ####
--
-- PostgreSQL's SECURITY DEFINER hardening guidance is to pin search_path to
-- exactly what the function needs and nothing the caller controls. Omitting
-- pg_temp does not do that: an unqualified search_path implicitly searches
-- pg_temp FIRST for *relation* names (not functions/operators), ahead of
-- every schema actually listed. ledger_app holds the default TEMPORARY
-- privilege, so it can CREATE TEMP TABLE journal_entries_default (or
-- journal_entries) in its own session and have this function's unqualified
-- references resolve to that shadow relation instead of the real one.
--
-- The review found this bounded, not a data-corruption path: the two
-- unqualified relation names each function touches (journal_entries,
-- journal_entries_default) are used in ALTER TABLE ... DETACH/ATTACH
-- PARTITION, which requires the named relation to actually BE a partition
-- of/attached to the other -- a caller-created temp table never is, so the
-- statement errors and the whole SECURITY DEFINER call (one implicit
-- transaction) rolls back. The exploit therefore tops out at a self-inflicted
-- DoS of the caller's own session, not privilege escalation or data
-- corruption. Fixing it anyway: the next person who edits either function
-- body to add a step that does NOT require partition membership (a plain
-- SELECT or INSERT against an unqualified name, say) would inherit that
-- bound only by luck, not by anything this function declares. Explicitly
-- listing pg_temp last removes the shadowing vector regardless of what the
-- body does in the future.
ALTER FUNCTION ledger_create_monthly_partition(text, date, date)
    SET search_path = public, pg_temp;
ALTER FUNCTION ledger_rebalance_default_partition(date, date)
    SET search_path = public, pg_temp;

-- ####  m-2: ledger_create_monthly_partition did not check (p_from, p_to)
-- ####  against p_name  ####
--
-- The function's only integrity check on its arguments was p_name's shape
-- (the `^journal_entries_y[0-9]{4}m[0-9]{2}$` regex). p_from/p_to were free:
-- ledger_create_monthly_partition('journal_entries_y2027m01', '2027-01-01',
-- '2027-01-02') creates a partition NAMED january but covering one day.
-- EnsureMonthlyPartitions (postgres/partition_store.go) checks coverage via
-- to_regclass on the expected name, sees the January partition already
-- exists, and moves on -- so Jan 3-31 silently falls into the default
-- partition every month thereafter, and ledger_rebalance_default_partition's
-- later attempt to move those rows into their monthly home fails outright
-- ("no partition found for row", since the partition covering most of
-- January genuinely does not exist) and rolls back the whole rebalance.
-- ledger_app cannot self-heal (DROP/ALTER PARTITION needs ledger_owner), so
-- this is a persistent availability defect requiring manual DBA
-- intervention, not a one-time hiccup. No caller in this codebase can reach
-- p_from/p_to except through EnsureMonthlyPartitions' own derivation from
-- p_name (always month-aligned), so this closes a defense-in-depth gap
-- against a caller this function does not currently have -- consistent with
-- the same posture the p_name regex itself already takes ("constraining its
-- shape here ... is what makes that safe against a caller that is not
-- partition_store.go", per migration 007's header).
CREATE OR REPLACE FUNCTION ledger_create_monthly_partition(
    p_name text, p_from date, p_to date
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_year        int;
    v_month       int;
    v_expected_to date;
BEGIN
    IF p_name !~ '^journal_entries_y[0-9]{4}m[0-9]{2}$' THEN
        RAISE EXCEPTION 'ledger: invalid monthly partition name %', p_name
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    v_year := substring(p_name from 'y([0-9]{4})m')::int;
    v_month := substring(p_name from 'm([0-9]{2})$')::int;
    IF v_month < 1 OR v_month > 12 THEN
        RAISE EXCEPTION 'ledger: invalid month in partition name %', p_name
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- make_date(v_year, v_month, 1) is always the first of the named month;
    -- the partition must cover exactly that month, no more, no less.
    v_expected_to := (make_date(v_year, v_month, 1) + INTERVAL '1 month')::date;
    IF p_from <> make_date(v_year, v_month, 1) OR p_to <> v_expected_to THEN
        RAISE EXCEPTION 'ledger: partition name % does not match its range [%, %) -- expected [%, %)',
            p_name, p_from, p_to, make_date(v_year, v_month, 1), v_expected_to
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
