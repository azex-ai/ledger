-- Two Critical findings from the 2026-09-03 independent review
-- (`docs/audits/2026-09-03-independent-review/install-roles.md` C1 and C2).
-- Both live in one function, and both let `ledger_app` -- holding no DDL, no
-- ownership and no DELETE anywhere (I-22, and re-measured statement by
-- statement in that report's appendix B) -- commit a one-sided journal entry
-- and mint 999,999 out of nothing.
--
--   C1  `check_journal_currency_balance()` is SECURITY INVOKER with an empty
--       `proconfig`, and its body says `FROM journal_entries` unqualified.
--       PostgreSQL searches `pg_temp` FIRST for *relation* names whenever
--       pg_temp is not itself listed in search_path. `ledger_app` holds the
--       default TEMPORARY privilege, so one `CREATE TEMP TABLE
--       journal_entries (...)` makes the deferred balance trigger aggregate
--       over the attacker's empty shadow table. Measured: the unbalanced
--       COMMIT succeeds; without the temp table the identical pair of INSERTs
--       is refused with "journal N has unbalanced entries by currency".
--
--   C2  the same function deduped by journal_id inside a `pg_temp` table it
--       created with `CREATE TEMP TABLE IF NOT EXISTS`. The caller can create
--       that table first -- same name, default `ON COMMIT PRESERVE ROWS` --
--       and pre-fill it with `generate_series(1, 10000)`; `journals_id_seq`
--       is readable by `ledger_app`, so the ids are predictable. Every
--       subsequent INSERT hits `ON CONFLICT DO NOTHING`, `NOT FOUND` is true,
--       and the aggregate never runs at all. Independent of C1: pinning
--       search_path does not help, because a temp table can only ever live in
--       pg_temp.
--
-- Nine SECURITY INVOKER guard functions had `proconfig = (none)`; migration
-- 013 pinned only the two SECURITY DEFINER partition functions, and the gate
-- it shipped (`TestPartitionFunctions_SearchPathIncludesPgTemp`) enumerated
-- exactly those two by name, so no amount of running it could have found this.
-- That gate is replaced by a catalogue-derived one in
-- `postgres/guard_function_search_path_test.go`.

------------------------------------------------------------------------------
-- 1. One search_path form for every function in this schema
------------------------------------------------------------------------------
--
-- The form is `public, pg_temp` -- pg_temp present, and LAST. Two measured
-- facts decide that, and both contradict the shape this fix was first
-- specified with (`pg_catalog, public`, pg_temp omitted):
--
--   (a) Omitting pg_temp does not exclude it, it PROMOTES it. Measured on
--       postgres:17.10 with `public.t` holding 3 rows and a session-local
--       `CREATE TEMP TABLE t` holding none:
--
--         proconfig                            body        count(*)
--         (none)                               FROM t      0   <- shadowed
--         search_path=pg_catalog, public       FROM t      0   <- shadowed
--         search_path=pg_catalog, public, pg_temp  FROM t  3
--         search_path=pg_catalog, public       FROM public.t  3
--
--       Migration 013's header already stated this in prose ("an unqualified
--       search_path implicitly searches pg_temp FIRST for *relation* names");
--       the numbers above are that sentence re-measured before relying on it.
--
--   (b) Listing pg_catalog first breaks unqualified DDL, because an
--       unqualified CREATE lands in the first schema on the path:
--       `ERROR: permission denied to create "pg_catalog.ddl_probe"`.
--       `ledger_create_monthly_partition` does exactly that
--       (`EXECUTE 'CREATE TABLE %I PARTITION OF journal_entries ...'`), so a
--       pg_catalog-first path would have turned partition maintenance into an
--       outage. pg_catalog is searched first anyway when it is not named;
--       naming it buys nothing and costs that.
--
-- SECURITY DEFINER and SECURITY INVOKER get the SAME form on purpose. The
-- shadowing vector is a property of relation-name resolution, not of who the
-- function runs as; splitting the forms would mean two rules to remember and
-- a gate that cannot be an equality check.
ALTER FUNCTION ledger_block_mutation()                  SET search_path = public, pg_temp;
ALTER FUNCTION ledger_block_column_mutation()           SET search_path = public, pg_temp;
ALTER FUNCTION ledger_journals_block_arbitrary_update() SET search_path = public, pg_temp;
ALTER FUNCTION ledger_classifications_guard()           SET search_path = public, pg_temp;
ALTER FUNCTION ledger_reservations_guard()              SET search_path = public, pg_temp;
ALTER FUNCTION ledger_account_policies_guard()          SET search_path = public, pg_temp;
ALTER FUNCTION ledger_bookings_guard()                  SET search_path = public, pg_temp;
ALTER FUNCTION ledger_events_guard()                    SET search_path = public, pg_temp;
ALTER FUNCTION ledger_reject_unknown_normal_side(text)  SET search_path = public, pg_temp;
ALTER FUNCTION ledger_resweep_ownership()               SET search_path = public, pg_temp;

-- `ledger_signed_amount` and `ledger_signed_delta` are deliberately NOT
-- pinned, and the new gate names them as its only exemption. They are
-- LANGUAGE sql IMMUTABLE one-liners over their arguments -- no relation
-- reference to shadow -- and a `SET` clause makes a SQL function
-- **un-inlinable**, which is not a micro-cost here: they sit inside the
-- balance, rollup, holder and trend aggregations. Measured over 50,000 rows
-- on postgres:17.10:
--
--   inlined (no SET):     Execution Time 3.770 ms   plan shows the CASE
--   pinned  (SET ...):    Execution Time 31.453 ms  plan shows a function call
--
-- 8.3x, for a function with nothing to protect. The gate enforces the shape
-- of that exemption rather than trusting this comment: exempt functions must
-- be LANGUAGE sql, IMMUTABLE, not SECURITY DEFINER, referenced by no trigger,
-- and their source must contain no FROM/JOIN/INSERT/UPDATE/DELETE -- so the
-- exemption cannot later be widened to cover something that reads a table.

------------------------------------------------------------------------------
-- 2. The balance guard stops keeping state anywhere the caller can write
------------------------------------------------------------------------------
--
-- The dedup existed for a real reason, restated from 001's header: Postgres
-- constraint triggers must be FOR EACH ROW (statement-level constraint
-- triggers do not exist), so an N-entry journal fires the check N times and a
-- naive implementation re-aggregates the whole journal each time -- O(N^2).
-- Dropping the dedup outright was measured, on the real schema, through the
-- real `PostJournal` path (median of 11, fresh database per variant):
--
--   entries/journal        2      6     20     100     500    2000
--   temp-table dedup    3.31   3.97   6.74   22.27  100.85  397.08  ms
--   no dedup            2.92   3.47   6.43   26.09  211.81 2268.63  ms
--   this migration      3.35   3.71   6.13   20.81   96.78  363.61  ms
--
-- So "just drop it" is free at the sizes presets actually post (2-6 entries) and
-- 5.7x at 2000 -- and 2000 entries is one INSERT loop away for the very
-- credential this guard exists to contain, which makes the quadratic a lever
-- rather than a footnote. Hence a dedup, but one keyed on something the
-- caller cannot write.
--
-- The replacement moves the aggregate to where "once per journal" is a
-- property of the schema rather than of a memo:
--
--   * A deferred constraint trigger on `journals` runs the aggregate exactly
--     once per journals row this transaction wrote. No dedup structure, no
--     state, nothing to pre-seed.
--   * The per-entry trigger stays as the backstop for entries appended to a
--     journal THIS transaction did not write -- which is the direct-SQL
--     tampering shape -- and skips otherwise. Its skip predicate is
--     `journals.xmin = pg_current_xact_id()::xid`: a system column, not
--     writable by anyone, and safe in the only direction that matters --
--     a wrong answer means the aggregate runs again (slower), never that it
--     is skipped for a row this transaction did not write.
--
-- Why the journals trigger fires on UPDATE as well as INSERT: `xmin` is
-- refreshed by UPDATE too. `journals.event_id` is backfillable by `ledger_app`
-- (the one column `journals_no_arbitrary_update` lets through), so an
-- INSERT-only trigger would leave this bypass: UPDATE an old journal's
-- event_id to take ownership of its xmin, then append a one-sided entry --
-- the entry trigger sees "this transaction wrote that journal" and skips,
-- and no INSERT ever happened for the journals trigger to fire on. Covering
-- UPDATE makes the skip predicate exact: `journals.xmin = my xid` is true
-- only if this transaction INSERTed or UPDATEd that row, and either one
-- queues the aggregate.

-- The aggregate itself, named once so the two triggers cannot drift apart.
-- SECURITY INVOKER: it reads `journal_entries`, which every caller that can
-- reach a trigger on that table can already read.
CREATE FUNCTION ledger_assert_journal_balanced(p_journal_id BIGINT) RETURNS void
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM public.journal_entries
        WHERE journal_id = p_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END
        ) <> 0
    ) THEN
        RAISE EXCEPTION 'journal % has unbalanced entries by currency', p_journal_id
            USING
                ERRCODE = '23514',
                CONSTRAINT = 'chk_journal_currency_balance';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    target_journal_id BIGINT;
    written_here      BOOLEAN;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    -- Did this transaction write the journals row itself? If so
    -- trg_check_journal_balance_on_journal is already queued for it and will
    -- run the aggregate once, whatever this transaction does to the entries.
    SELECT j.xmin = pg_catalog.pg_current_xact_id()::xid
      INTO written_here
      FROM public.journals j
     WHERE j.id = target_journal_id;

    IF COALESCE(written_here, FALSE) THEN
        RETURN NULL;
    END IF;

    PERFORM public.ledger_assert_journal_balanced(target_journal_id);
    RETURN NULL;
END;
$$;

CREATE FUNCTION ledger_check_journal_balance() RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    PERFORM public.ledger_assert_journal_balanced(NEW.id);
    RETURN NULL;
END;
$$;

ALTER FUNCTION ledger_assert_journal_balanced(BIGINT) OWNER TO ledger_owner;
ALTER FUNCTION ledger_check_journal_balance()         OWNER TO ledger_owner;

-- Migration 021 revoked PUBLIC EXECUTE across the catalogue; a new function is
-- EXECUTE-able by PUBLIC again unless it says otherwise, and
-- function_acl_test.go asserts each role's EXECUTE set is exactly the reviewed
-- whitelist.
REVOKE ALL ON FUNCTION ledger_assert_journal_balanced(BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION ledger_check_journal_balance()         FROM PUBLIC;

-- ledger_app needs EXECUTE on the aggregate, and this is not a loosening --
-- it is the difference between the guard running and the guard failing on the
-- wrong error. A trigger function's own EXECUTE right is checked at CREATE
-- TRIGGER time, but a function it CALLS is checked at call time against the
-- invoker, and both triggers here are SECURITY INVOKER. Without this grant
-- every ledger_app write raised `permission denied for function
-- ledger_assert_journal_balanced` -- fail-closed, so no money moved, but the
-- balance check never ran and the application was simply broken. Caught by
-- the C1/C2 pins, which run as ledger_app rather than on the superuser test
-- connection.
--
-- What the grant hands over is a function that reads journal_entries (which
-- ledger_app can already SELECT) and either returns nothing or raises. There
-- is no state to change and nothing to learn that a plain SELECT would not
-- tell the same caller.
GRANT EXECUTE ON FUNCTION ledger_assert_journal_balanced(BIGINT) TO ledger_app;

CREATE CONSTRAINT TRIGGER trg_check_journal_balance_on_journal
    AFTER INSERT OR UPDATE ON journals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_check_journal_balance();

------------------------------------------------------------------------------
-- 3. TEMPORARY is withdrawn from PUBLIC
------------------------------------------------------------------------------
--
-- Sections 1 and 2 close C1 and C2 on their own; this is the layer under
-- them, and it is affordable because after section 2 nothing in this
-- repository creates a temporary table any more. Grepped before revoking:
-- `postgres/*.go`, `postgres/sql/queries/*.sql` and the generated `sqlcgen`
-- have no CREATE TEMP anywhere; the only production occurrence was the dedup
-- set section 2 just deleted. Two test sites remain
-- (`postgres/partition_store_test.go`), and both run on the superuser test
-- connection, which is not subject to this ACL.
--
-- Two things about the mechanics are worth stating, because neither is
-- obvious and both were measured:
--
--   * `REVOKE ... FROM ledger_app` would be a no-op. TEMPORARY reaches
--     ledger_app through PUBLIC, and a privilege held via PUBLIC can only be
--     revoked from PUBLIC.
--   * This migration runs as `ledger_owner` (postgres/migrate.go pins one
--     connection and issues `SET ROLE ledger_owner` on it), and ledger_owner
--     does not own the database, so it cannot revoke a database privilege.
--     `SET LOCAL ROLE NONE` drops back to the session user -- the migration
--     credential, which the RUNBOOK's main path has owning the database --
--     for the rest of this transaction only; COMMIT restores ledger_owner
--     without a statement having to be reached to make that true.
--
-- The DO block is idempotent (already-revoked is a pass, not an error) and
-- fail-closed: a credential that can neither observe the revoke nor perform
-- it stops the migration with the exact statement a DBA has to run, rather
-- than logging a warning nobody reads. "Not run" is never folded into "done".
SET LOCAL ROLE NONE;

DO $$
DECLARE
    public_has_temp BOOLEAN;
BEGIN
    SELECT (d.datacl IS NULL)
        OR EXISTS (
               SELECT 1 FROM pg_catalog.aclexplode(d.datacl) a
               WHERE a.grantee = 0 AND a.privilege_type = 'TEMPORARY'
           )
      INTO public_has_temp
      FROM pg_catalog.pg_database d
     WHERE d.datname = pg_catalog.current_database();

    IF NOT public_has_temp THEN
        RETURN;   -- already withdrawn (re-run, or a DBA did it by hand)
    END IF;

    BEGIN
        EXECUTE pg_catalog.format(
            'REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC',
            pg_catalog.current_database());
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE EXCEPTION
            'ledger: this migration credential (%) cannot revoke TEMPORARY on database % -- only the database owner or a superuser can',
            pg_catalog.current_user, pg_catalog.current_database()
            USING HINT = pg_catalog.format(
                'Have a superuser or the database owner run: REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC; then re-run the migration. '
                || 'TEMPORARY is what lets a leaked application credential create pg_temp relations, which is the vector migration 030 closes.',
                pg_catalog.current_database());
    END;
END $$;
