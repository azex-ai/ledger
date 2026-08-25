-- 001_baseline.down.sql -- undo 001_baseline.up.sql completely.
--
-- Order mirrors up.sql in reverse, and for the same reasons:
--
--   1. PUBLIC's schema USAGE goes back FIRST. Every statement below names
--      public.<something> and needs the ACL-gated USAGE check to pass; the
--      runner's own explicit grant from up.sql would do, but restoring PUBLIC
--      first means this file works whether or not that grant survived.
--   2. Ownership comes back to the runner BEFORE anything is dropped. DROP,
--      ALTER and TRUNCATE are ownership-gated, not grant-gated, so a
--      non-superuser runner cannot drop a table ledger_owner owns no matter
--      what it has been granted.
--   3. Roles go last. A role cannot be dropped while it owns an object or
--      holds a privilege anywhere, so both have to be undone first.
--
-- ⚠️ Roles are cluster-wide, not per-database. up.sql creates the three roles
-- only if they are absent, so on a cluster where another database already used
-- them this file drops roles it did not create. `DROP OWNED BY` only cleans up
-- the CURRENT database, so if another database still holds objects owned by
-- ledger_owner the DROP ROLE below fails and this rollback stops with an
-- error. That is the intended behaviour: failing loudly beats half-tearing
-- down a security boundary and reporting success. A shared cluster wanting a
-- different answer should drop the roles by hand and skip this file's last
-- section.
--
-- ⚠️ This is a rollback of the ledger's core security boundary, not a routine
-- operation. Treat it as a DBA action expected to run with real superuser
-- privileges; the non-superuser path is reasoned about above but is not the
-- exercised one.

------------------------------------------------------------
-- 1. Restore PUBLIC's schema-level USAGE.
--
-- Not restored: CREATE on public to PUBLIC. Postgres 15+ does not grant that
-- by default either, so restoring it would not be undoing anything up.sql
-- actually took away from a from-scratch database.
------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO PUBLIC;

------------------------------------------------------------
-- 2. Reclaim ownership of everything from ledger_owner.
--
-- `GRANT ledger_owner TO runner` relies on the ADMIN OPTION Postgres
-- unconditionally gives the creator of a role -- the same relationship up.sql
-- used to hand ownership over in the first place.
--
-- The sequence loop skips sequences already back with the runner (the
-- BIGSERIAL-linked ones follow their table), symmetric with up.sql and for the
-- same reason: re-altering an already-correct owner is an error, not a no-op.
--
-- Routines are included here for the same reason up.sql includes them: the
-- guard functions are owned objects, and leaving them behind owned by a role
-- this file is about to drop would make the DROP ROLE fail.
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

    FOR r IN SELECT sequencename FROM pg_sequences
             WHERE schemaname = 'public' AND sequenceowner <> runner LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO %I', r.sequencename, runner);
    END LOOP;

    FOR r IN SELECT c.relname, c.relkind
             FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relkind IN ('v', 'm') LOOP
        IF r.relkind = 'v' THEN
            EXECUTE format('ALTER VIEW public.%I OWNER TO %I', r.relname, runner);
        ELSE
            EXECUTE format('ALTER MATERIALIZED VIEW public.%I OWNER TO %I', r.relname, runner);
        END IF;
    END LOOP;

    FOR r IN SELECT p.oid::regprocedure AS sig
             FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
             WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p') LOOP
        EXECUTE format('ALTER ROUTINE %s OWNER TO %I', r.sig, runner);
    END LOOP;

    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;

------------------------------------------------------------
-- 3. Drop the schema.
--
-- One statement with CASCADE, because the foreign keys form a cycle
-- (journals -> events -> bookings -> journals) and no drop order untangles it.
-- CASCADE also takes the triggers and the partitions of journal_entries.
--
-- schema_migrations is deliberately NOT dropped: golang-migrate owns it and
-- writes its own version row immediately after this file runs.
------------------------------------------------------------
DROP TABLE IF EXISTS
    entry_attestations,
    ledger_attestations,
    checkpoint_rebuilds,
    reconcile_scan_cursors,
    registration_rescans,
    ingest_dead_letters,
    chain_cursors,
    deposit_addresses,
    webhook_nonces,
    webhook_subscribers,
    period_closes,
    account_policy_changes,
    account_policies,
    withdrawals,
    deposits,
    events,
    bookings,
    reservation_settlement_legs,
    reservations,
    system_rollups,
    balance_snapshots,
    rollup_queue,
    balance_checkpoints,
    journal_entries,
    journals,
    entry_template_lines,
    entry_templates,
    journal_types,
    classifications,
    currencies
CASCADE;

DROP FUNCTION IF EXISTS check_journal_currency_balance();
DROP FUNCTION IF EXISTS ledger_reservations_guard();
DROP FUNCTION IF EXISTS ledger_classifications_guard();
DROP FUNCTION IF EXISTS ledger_journals_block_arbitrary_update();
DROP FUNCTION IF EXISTS ledger_block_mutation();

------------------------------------------------------------
-- 4. Undo the default-privilege entry that benefits ledger_owner. Not covered
--    by DROP OWNED BY: that removes default privileges created FOR a role,
--    and this entry was created for the runner, merely naming ledger_owner as
--    the beneficiary. Left behind, it blocks the DROP ROLE below.
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON TABLES FROM ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ledger_owner', runner);
END $$;

------------------------------------------------------------
-- 5. Drop the roles.
--
-- Only ledger_ro's SELECT on schema_migrations should still exist at this
-- point (every other grant went with its table in step 3). DROP OWNED BY
-- clears it and anything else missed, in this database.
--
-- The runner's own SELECT/INSERT/TRUNCATE grant on schema_migrations is
-- deliberately left in place. It is redundant now that ownership is back --
-- but revoking a privilege from the OWNER of a table really does take it away
-- (owner rights are ACL entries, not an ownership bypass), which would leave a
-- non-superuser runner unable to write its own version row. An earlier
-- iteration of this rollback did exactly that and got away with it only
-- because the tests connect as a superuser.
------------------------------------------------------------
REVOKE CREATE ON SCHEMA public FROM ledger_owner;
REVOKE USAGE ON SCHEMA public FROM ledger_owner, ledger_app, ledger_ro;

DROP OWNED BY ledger_owner, ledger_app, ledger_ro;

DROP ROLE IF EXISTS ledger_owner;
DROP ROLE IF EXISTS ledger_app;
DROP ROLE IF EXISTS ledger_ro;
