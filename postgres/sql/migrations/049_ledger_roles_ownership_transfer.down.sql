-- 049_ledger_roles_ownership_transfer.down.sql
--
-- Reverses 049_ledger_roles_ownership_transfer.up.sql: reassigns ownership
-- of every table/sequence back to whoever runs this rollback, and restores
-- PUBLIC's USAGE on the schema.
--
-- ============================================================================
-- Only run this in the same coordinated release window that reverts the
-- DATABASE_URL cutover back off of ledger_owner/ledger_app. Running it while
-- the migration Job's DATABASE_URL is still pointed at ledger_owner just
-- transfers ownership from ledger_owner to ledger_owner (a no-op) --
-- correct rollback requires the connection running this file to be the
-- pre-cutover identity 049.up.sql took ownership away from.
--
-- This file's `GRANT ledger_owner TO <runner>` relies on the same
-- permanent ADMIN OPTION 049.up.sql's step 2 uses (PostgreSQL unconditionally
-- grants the creator of a role admin option on it) -- untested here under a
-- non-superuser connection, so treat this as an emergency DBA operation
-- expected to run with actual superuser privileges, not something the
-- routine migration-job identity is verified to ever do to itself. This is
-- a deliberate scope boundary: rolling back the ledger's core DB security
-- boundary is not a routine, frequently-exercised path.
-- ============================================================================

------------------------------------------------------------
-- 1. Reclaim ownership of every table/sequence from ledger_owner.
------------------------------------------------------------
DO $$
DECLARE
    runner text := current_user;
    r RECORD;
BEGIN
    EXECUTE format('GRANT ledger_owner TO %I', runner);

    FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO %I', r.tablename, runner);
    END LOOP;
    -- Skip sequences already cascaded back to `runner` by the table loop
    -- above (SERIAL/BIGSERIAL-linked ones) -- see up.sql's matching comment
    -- for why re-altering an already-correct owner fails under anything
    -- less than superuser. Superuser (this file's documented expectation)
    -- would not hit that error either way, but the filter keeps this
    -- symmetric with up.sql and correct if that assumption ever changes.
    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequenceowner <> runner LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO %I', r.sequencename, runner);
    END LOOP;

    -- Clean up the two residual, narrow grants up.sql's step 2 added so the
    -- connecting role could keep running migrations: SCHEMA-level USAGE and
    -- the schema_migrations table grant (cleaned up separately below, since
    -- it is checked via a different catalog view). Both are redundant now
    -- that ownership is restored to `runner` above (ownership already
    -- implies them) -- drop the explicit grant rather than leave a stale
    -- ACL entry naming a specific role. Assumes (per this file's header)
    -- that `runner` here is the same identity up.sql granted these to; a
    -- different superuser rolling this back would need to clean up that
    -- original role's schema USAGE grant manually.
    EXECUTE format('REVOKE USAGE ON SCHEMA public FROM %I', runner);

    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;

------------------------------------------------------------
-- 2. Clean up the other residual ACL entry from up.sql's step 2: the
--    explicit SELECT/INSERT/TRUNCATE grant on schema_migrations that lets
--    golang-migrate's own bookkeeping keep working for the connecting role.
------------------------------------------------------------
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT DISTINCT grantee
        FROM information_schema.role_table_grants
        WHERE table_schema = 'public' AND table_name = 'schema_migrations'
          AND grantee NOT IN ('ledger_owner', 'ledger_app', 'ledger_ro', 'PUBLIC')
    LOOP
        EXECUTE format('REVOKE ALL ON public.schema_migrations FROM %I', r.grantee);
    END LOOP;
END $$;

------------------------------------------------------------
-- 3. Restore PUBLIC's schema-level USAGE. Not restored: CREATE on public to
--    PUBLIC -- modern Postgres (15+) does not grant that by default either,
--    so restoring it here would not be undoing anything up.sql actually
--    took away from a from-scratch PG15+ database.
------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO PUBLIC;
