-- 042_ledger_roles.up.sql
--
-- P1 -- DB role least-privilege.
-- Design: docs/plans/2026-08-21-tamper-evident-ledger-design.md §3 (threat
--   model §1, attack path A6).
-- Contract: docs/plans/2026-08-21-integrity-hardening-contracts.md §1/§5/§8/§9.
--
-- Expand phase only (deployment.md): creates three roles and grants
-- least-privilege access. Does NOT switch DATABASE_URL -- whatever role
-- every environment connects with today keeps working exactly as before,
-- because it either IS the bootstrap/superuser identity or remains the
-- owner of everything until the explicit ownership transfer at the very end
-- of this file (and GRANT never removes a role's own standing privileges).
-- A later release ("migrate") points the serving DATABASE_URL at ledger_app
-- and the migration-job URL at ledger_owner; "contract" retires the
-- bootstrap credential from daily use. See docs/RUNBOOK.md §9 and
-- deploy/helm/ledger/README.md.
--
-- Also closes A6: docs/RUNBOOK.md's emergency-stop section has referenced
-- `ledger_app` since it was written, but no migration ever created it --
-- the runbook was instructing operators to use a role that didn't exist.
--
-- Role shape:
--   ledger_owner -- owns every table/sequence; the only role with DDL
--                   (ALTER/DROP/TRUNCATE/trigger management). Postgres does
--                   not let GRANT confer those rights -- only ownership (or
--                   superuser) does, so "ledger_owner has DDL" is only true
--                   because it actually owns the objects (step 6 below).
--   ledger_app   -- SELECT/INSERT/UPDATE on ordinary tables; SELECT/INSERT
--                   ONLY on journal_entries (parent + every partition) --
--                   the running application never updates a posted entry.
--                   No DELETE anywhere. No DDL of any kind.
--   ledger_ro    -- SELECT everywhere (Metabase/BI/reporting). This is the
--                   role the 2026-05 credential-leak incident should have
--                   used instead of a superuser session.

------------------------------------------------------------
-- 1. Create the three roles (idempotent). No passwords are set here --
--    secrets never enter migration files or git (infra.md); an operator
--    sets them out-of-band when cutting over in the "migrate" phase.
------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner') THEN
        CREATE ROLE ledger_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_app') THEN
        CREATE ROLE ledger_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_ro') THEN
        CREATE ROLE ledger_ro LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END $$;

------------------------------------------------------------
-- 2. Schema-level lockdown: PUBLIC gets nothing implicit. ledger_owner
--    additionally gets CREATE -- it is the only role future migrations will
--    ever run DDL as.
------------------------------------------------------------
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO ledger_owner, ledger_app, ledger_ro;
GRANT CREATE ON SCHEMA public TO ledger_owner;

------------------------------------------------------------
-- 3. ledger_app grants. Issued BEFORE the ownership transfer in step 6 --
--    GRANT requires the executing role to currently own the object (or be
--    superuser); once ownership moves to ledger_owner that would no longer
--    hold for a non-superuser migration runner.
--
--    journal_entries is partitioned (migration 037): grant the parent AND
--    every partition that exists right now explicitly. Do not assume a
--    GRANT on the parent alone reaches partitions Postgres hasn't attached
--    yet at grant time -- postgres/roles_test.go pins the actual behavior
--    (a partition PartitionService creates AFTER this migration runs) by
--    connecting as ledger_app and inserting into it.
--
--    schema_migrations (created by golang-migrate itself, not by any file
--    here) is deliberately excluded: ledger_app has no legitimate reason to
--    read or write the applied-migrations ledger.
------------------------------------------------------------
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('r', 'p')
          AND NOT c.relispartition
          AND c.relname NOT IN ('journal_entries', 'schema_migrations')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON public.%I TO ledger_app', r.relname);
    END LOOP;

    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('r', 'p')
          AND (c.relname = 'journal_entries' OR c.relispartition)
    LOOP
        EXECUTE format('GRANT SELECT, INSERT ON public.%I TO ledger_app', r.relname);
    END LOOP;

    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('GRANT USAGE, SELECT ON public.%I TO ledger_app', r.sequencename);
    END LOOP;
END $$;

------------------------------------------------------------
-- 4. ledger_ro: read-only everywhere. No aggregate/reporting views exist yet
--    to scope this down to further (tracked as a RUNBOOK follow-up) --
--    full-schema SELECT is still strictly less than the superuser session a
--    BI tool has no business holding.
------------------------------------------------------------
GRANT SELECT ON ALL TABLES IN SCHEMA public TO ledger_ro;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO ledger_ro;

------------------------------------------------------------
-- 5. Any table/sequence created from here on by whoever runs this migration
--    (today's bootstrap role; ledger_owner itself after cutover) also grants
--    full rights to ledger_owner automatically, so future migrations never
--    have to remember this step. Deliberately the ONLY default-privilege
--    entry this migration adds -- ledger_app/ledger_ro get nothing
--    automatically on new objects, forcing an explicit, reviewable GRANT in
--    whichever future migration introduces them (contracts.md §9 point 3).
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON TABLES TO ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON SEQUENCES TO ledger_owner', runner);
END $$;

------------------------------------------------------------
-- 6. ledger_owner becomes the real owner of every existing table and
--    sequence. `GRANT role TO runner` first: transferring ownership
--    requires the executing role to be able to SET ROLE to the new owner,
--    which plain membership provides without needing ADMIN OPTION or
--    superuser. The role is only ever a member of ledger_owner for the
--    duration of this block.
------------------------------------------------------------
DO $$
DECLARE
    runner text := current_user;
    r RECORD;
BEGIN
    EXECUTE format('GRANT ledger_owner TO %I', runner);

    FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO ledger_owner', r.tablename);
    END LOOP;
    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ledger_owner', r.sequencename);
    END LOOP;

    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;
