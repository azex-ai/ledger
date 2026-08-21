-- 042_ledger_roles.up.sql
--
-- P1 -- DB role least-privilege.
-- Design: docs/plans/2026-08-21-tamper-evident-ledger-design.md §3 (threat
--   model §1, attack path A6).
-- Contract: docs/plans/2026-08-21-integrity-hardening-contracts.md §1/§5/§8/§9.
--
-- Pure expand phase (deployment.md): creates three roles and grants them
-- least-privilege access. Every statement below is ADDITIVE ONLY -- nothing
-- is revoked, and no existing table or sequence changes owner. Whatever
-- role every environment connects with today keeps ALL of its standing
-- privileges, byte for byte, after this migration commits.
--
-- That is a deliberate, hard boundary, not an incidental one: an earlier
-- version of this migration also ran `REVOKE ALL ON SCHEMA public FROM
-- PUBLIC` and transferred every table/sequence's ownership to
-- `ledger_owner` in the same file. It passed every test that connected as
-- the new roles, because testcontainers' bootstrap user and this repo's
-- docker-compose POSTGRES_USER are both real Postgres superusers, which
-- bypass ownership/ACL checks entirely -- so those tests couldn't see that
-- the *migration-running connection itself* had just lost every privilege
-- it had via ownership or PUBLIC's default schema access, with no
-- replacement grant, the moment the migration committed. Managed Postgres
-- master users (RDS, Cloud SQL, Neon, ...) typically are NOT real
-- superusers, so on a real production database that version would have
-- locked the very next write out of its own database -- proven by
-- postgres.TestMigration042_DoesNotStrandTheMigrationRunner failing against
-- that version with `permission denied for table schema_migrations`
-- (golang-migrate's own version-bookkeeping write, ordered by Postgres
-- after the migration's ownership transfer already committed).
--
-- The REVOKE + ownership-transfer half of the original design belongs to a
-- separate, later "migrate" migration that MUST ship in the same release as
-- the DATABASE_URL cutover (deployment.md's migrate phase; see
-- docs/RUNBOOK.md §9). This file only ever reaches the "migrate" phase's
-- prerequisite: the roles exist and hold the grants they will need, but
-- nothing is locked down and nothing changes owner yet.
--
-- Also closes A6: docs/RUNBOOK.md's emergency-stop section has referenced
-- `ledger_app` since it was written, but no migration ever created it --
-- the runbook was instructing operators to use a role that didn't exist.
--
-- Role shape (target end-state, reached over expand -> migrate -> contract,
-- NOT by this migration alone):
--   ledger_owner -- will own every table/sequence once the "migrate"
--                   migration runs; the only role with DDL
--                   (ALTER/DROP/TRUNCATE/trigger management). Postgres does
--                   not let GRANT confer those rights -- only ownership (or
--                   superuser) does.
--   ledger_app   -- SELECT/INSERT/UPDATE on ordinary tables; SELECT/INSERT
--                   ONLY on journal_entries (parent + every partition) --
--                   the running application never updates a posted entry.
--                   No DELETE anywhere. No DDL of any kind. These grants
--                   are already live after THIS migration -- ledger_app is
--                   fully usable today, independent of the ownership
--                   transfer.
--   ledger_ro    -- SELECT everywhere (Metabase/BI/reporting). This is the
--                   role the 2026-05 credential-leak incident should have
--                   used instead of a superuser session. Also already live
--                   after this migration.

------------------------------------------------------------
-- 1. Create the three roles (idempotent). No passwords are set here --
--    secrets never enter migration files or git (infra.md); an operator
--    sets them out-of-band when cutting over in the "migrate" phase.
--
--    ledger_owner is created under `createrole_self_grant = 'set'` (scoped
--    to this transaction via SET LOCAL): since PostgreSQL 16, CREATEROLE
--    alone no longer implies any relationship to a role you create --
--    without this, a non-superuser connection with CREATEROLE (every
--    realistic managed-Postgres master user) would create ledger_owner and
--    then have NO membership in it at all, unable to even run `ALTER TABLE
--    ... OWNER TO ledger_owner`. 049 needs exactly this ("member WITH SET
--    OPTION", i.e. can `SET ROLE ledger_owner`) and nothing more --
--    deliberately WITHOUT INHERIT, so the connecting role never
--    automatically carries ledger_owner's privileges; it must explicitly
--    `SET ROLE ledger_owner` (and 049 does, only for the one statement that
--    needs it -- see 049's header). Reset immediately after so
--    ledger_app/ledger_ro do not also pick up a standing self-grant they
--    never use.
------------------------------------------------------------
SET LOCAL createrole_self_grant = 'set';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner') THEN
        CREATE ROLE ledger_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END $$;
SET LOCAL createrole_self_grant = DEFAULT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_app') THEN
        CREATE ROLE ledger_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_ro') THEN
        CREATE ROLE ledger_ro LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END $$;

------------------------------------------------------------
-- 2. Schema-level grants -- additive only. PUBLIC's existing access (if
--    any) is untouched; whoever the migration-running role turns out to be
--    keeps whatever schema access it already had. ledger_owner additionally
--    gets CREATE -- it will be the only role future migrations run DDL as,
--    once the "migrate" migration hands it real ownership.
------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO ledger_owner, ledger_app, ledger_ro;
GRANT CREATE ON SCHEMA public TO ledger_owner;

------------------------------------------------------------
-- 3. ledger_app grants. These work regardless of who currently owns the
--    tables (GRANT never requires the grantee to own anything, and issuing
--    it never revokes the grantor's own standing privileges) -- so
--    ledger_app is fully functional the moment this migration commits, with
--    zero dependency on the future ownership transfer.
--
--    journal_entries is partitioned (migration 037): grant the parent AND
--    every partition that exists right now explicitly. Do not assume a
--    GRANT on the parent alone reaches partitions Postgres hasn't attached
--    yet at grant time -- postgres/roles_test.go pins the actual behavior
--    (a partition created AFTER this migration runs) by connecting as
--    ledger_app and inserting into it.
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
--    to scope this down to further -- docs/RUNBOOK.md §9 tracks that as an
--    explicit follow-up (design doc §3 prefers views over full-table
--    SELECT); full-schema SELECT is still strictly less than the superuser
--    session a BI tool has no business holding.
------------------------------------------------------------
GRANT SELECT ON ALL TABLES IN SCHEMA public TO ledger_ro;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO ledger_ro;

------------------------------------------------------------
-- 5. Any table/sequence created from here on by whoever runs this migration
--    also grants full rights to ledger_owner automatically, so future
--    migrations never have to remember this step once ledger_owner starts
--    running them. Purely additive -- it changes what happens to FUTURE
--    objects, and does not touch the running role's own privileges.
--    Deliberately the ONLY default-privilege entry this migration adds --
--    ledger_app/ledger_ro get nothing automatically on new objects, forcing
--    an explicit, reviewable GRANT in whichever future migration introduces
--    them (contracts.md §9 point 3).
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON TABLES TO ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON SEQUENCES TO ledger_owner', runner);
END $$;

-- Deliberately NOT in this migration: REVOKE ALL ON SCHEMA public FROM
-- PUBLIC, and ALTER TABLE/SEQUENCE ... OWNER TO ledger_owner. Both are
-- destructive to whatever role runs migrations today and belong in the
-- "migrate" migration that ships alongside the DATABASE_URL cutover (see
-- this file's header and docs/RUNBOOK.md §9).
