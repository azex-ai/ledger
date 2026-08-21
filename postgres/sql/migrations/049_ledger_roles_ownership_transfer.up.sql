-- 049_ledger_roles_ownership_transfer.up.sql
--
-- P1 -- "migrate" phase (deployment.md). This is the destructive half of P1
-- that 042 (expand phase) deliberately left out: REVOKE ALL ON SCHEMA
-- public FROM PUBLIC, and transfer every existing table/sequence's
-- ownership to ledger_owner.
--
-- ============================================================================
-- MUST SHIP IN THE SAME RELEASE AS THE DATABASE_URL CUTOVER. DO NOT DEPLOY
-- THIS MIGRATION ON ITS OWN.
-- ============================================================================
--
-- This migration is EXPECTED to strand whatever role is currently connecting
-- as DATABASE_URL from every business table: after it commits, that role has
-- no ownership left (unless it happens to be ledger_owner) and no
-- PUBLIC-derived schema access either. That is the point, not a bug -- see
-- docs/plans/2026-08-21-tamper-evident-ledger-design.md §3 and
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §1. An earlier
-- version of 042 did this same thing bundled into the "expand" step and
-- called that a non-breaking change; it was not (see 042's header and
-- docs/RUNBOOK.md §9 for the incident this migration's split is fixing).
--
-- The release that deploys this migration must, in the same rollout:
--   1. Point the migration Job's DATABASE_URL secret key
--      (`migrations.job.databaseUrlKey`) at ledger_owner's credentials.
--   2. Point serving pods' DATABASE_URL at ledger_app's credentials
--      (already fully functional since 042 -- see docs/RUNBOOK.md §9).
--   3. Only then run this migration.
--
-- Whoever executes this specific migration file must currently own
-- everything (or be superuser), and must be the SAME role 042 created the
-- three roles as. Two role-membership facts 042 leaves in place make this
-- migration work under that role without superuser:
--   - `createrole_self_grant = 'set'` at 042's CREATE ROLE ledger_owner gave
--     that role standing "member WITH SET OPTION" of ledger_owner (can `SET
--     ROLE ledger_owner`, no automatic inherit) -- needed by step 1 below.
--   - PostgreSQL unconditionally grants the creator of a role ADMIN OPTION
--     on it (independent of createrole_self_grant, which only controls
--     INHERIT/SET) -- needed by step 2 below.
--
-- Postponed to a later "contract" phase, not this migration: fully retiring
-- the pre-042 admin/bootstrap credential from any use (deployment.md
-- "contract" step). This migration only moves ownership; it does not
-- require deleting the old credential. The one residual capability it
-- deliberately leaves behind (documented in step 2) is exactly what that
-- later phase should close.
--
-- ⚠️ Ordering is load-bearing, not stylistic -- everything that needs to
-- reference an object in the `public` schema happens BEFORE `REVOKE ALL ON
-- SCHEMA public FROM PUBLIC` runs. Two genuine deadlocks, both found by
-- writing postgres.TestMigration049_StrandsTheOldConnectionByDesign against
-- a non-superuser connection (docker-compose/testcontainers both connect as
-- real superusers, which silently bypass every check below -- see 042's
-- header and docs/RUNBOOK.md §9):
--
--   1. Schema ownership does not imply schema USAGE. `public` is owned by
--      the dynamic pseudo-role `pg_database_owner` (PG15+ default), and the
--      connecting role -- being this database's actual owner -- IS
--      recognized as its administrator for owner-gated actions (REVOKE ALL
--      ON SCHEMA, GRANT ON SCHEMA). But that does NOT extend to the regular
--      ACL-gated USAGE check every other statement needs just to reference
--      `public.<anything>` by name. Revoke PUBLIC's default USAGE before
--      finishing the ownership transfer, and the very next
--      `ALTER TABLE public.x OWNER TO ledger_owner` fails with
--      "permission denied for schema public" -- the connecting role locks
--      *itself* out mid-migration, before ledger_owner even has anything.
--
--   2. golang-migrate calls `SetVersion` TWICE per migration -- once to mark
--      the version dirty *before* running this file's SQL, and once to mark
--      it clean *after* (migrate.go's runMigrations, both calls on the same
--      connection but each its own transaction; the pgx/v5 driver's
--      SetVersion, database/pgx/v5/pgx.go:339-364, does `TRUNCATE
--      schema_migrations` then `INSERT ... (version, dirty)`). The second
--      call happens after this file's own transaction has already
--      committed. If schema_migrations's ownership (and the connecting
--      role's schema USAGE) is gone by then, that second call fails with
--      "permission denied for table schema_migrations" and golang-migrate
--      reports the whole migration as failed/dirty -- even though this
--      file's DDL already committed. Retrying does not help: the same role
--      runs the same file again and hits the same wall.
--
-- The fix for both: do all ownership transfers and the narrow
-- schema_migrations/schema-USAGE re-grants first, while PUBLIC's default
-- access is still in place; revoke it last, as the final statement.

------------------------------------------------------------
-- 1. ledger_owner becomes the real owner of every existing table and
--    sequence (schema_migrations included). This only needs the "member
--    WITH SET OPTION" relationship 042 established -- Postgres's OWNER TO
--    check is "does the current role already own this object, AND can it
--    SET ROLE to the new owner", not "is the current role currently
--    switched to the new owner". No ADMIN OPTION is needed for this step.
------------------------------------------------------------
DO $$
DECLARE
    runner text := session_user;
    r RECORD;
BEGIN
    FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO ledger_owner', r.tablename);
    END LOOP;
    -- `ALTER TABLE ... OWNER TO` cascades to any sequence a SERIAL/BIGSERIAL
    -- column auto-created (most of them) -- the loop above already
    -- transferred those. Re-altering an already-transferred sequence fails
    -- ("must be owner of sequence X"): `runner` only has SET, deliberately
    -- not INHERIT, on ledger_owner, so it does not satisfy the
    -- ownership-equivalence check Postgres uses once the sequence's actual
    -- owner is already ledger_owner. Filtering to sequenceowner <>
    -- 'ledger_owner' makes this idempotent and correct regardless of which
    -- sequences are auto-linked vs standalone.
    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequenceowner <> 'ledger_owner' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ledger_owner', r.sequencename);
    END LOOP;

    ------------------------------------------------------------
    -- 2. Carve out two narrow, deliberate exceptions: explicitly re-grant
    --    the connecting role exactly what golang-migrate's own bookkeeping
    --    needs -- USAGE on the schema (deadlock 1 above) and
    --    SELECT/INSERT/TRUNCATE on schema_migrations (deadlock 2 above).
    --    These are the ONLY things the old role keeps any access to after
    --    this migration -- every business table is still fully locked out.
    --
    --    Schema-level USAGE is granted by `runner` itself, while it is
    --    still the schema's effective administrator (owner-gated actions on
    --    a `pg_database_owner`-owned schema work for the database's actual
    --    owner -- see the header).
    --
    --    The schema_migrations grant must be issued while holding
    --    ledger_owner's privileges (it now owns that table, and `runner`
    --    holds no privilege on it otherwise). This temporarily upgrades
    --    `runner`'s existing membership to WITH INHERIT TRUE using its
    --    permanent ADMIN OPTION (see header), issues the grant, then
    --    revokes the membership again -- confirmed to fully remove BOTH the
    --    INHERIT row just added and the original SET-only row from 042,
    --    leaving only the untouched, non-functional ADMIN-only row Postgres
    --    grants automatically to any CREATEROLE-holding creator.
    --
    --    ⚠️ That ADMIN-only row is the one capability this migration
    --    deliberately cannot revoke (Postgres does not offer a way for a
    --    non-superuser to strip its own automatic admin option on a role it
    --    created): `runner` could always re-run this same GRANT ... WITH
    --    INHERIT TRUE / REVOKE dance again to regain temporary access to
    --    ledger_owner's privileges. This is the residual risk the future
    --    "contract" phase should close (rotate or fully retire this
    --    credential; a real superuser can strip the admin option directly).
    --    An earlier version of this migration used `SET ROLE
    --    ledger_owner` / `RESET ROLE` here instead -- it worked when tested
    --    standalone, but interacted badly with golang-migrate's two
    --    SetVersion calls around this file (deadlock 2 above) in a way that
    --    was not fully root-caused; the GRANT-based approach sidesteps
    --    role-switching entirely and is what's actually shipped.
    ------------------------------------------------------------
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', runner);

    EXECUTE format('GRANT ledger_owner TO %I WITH INHERIT TRUE', runner);
    EXECUTE format('GRANT SELECT, INSERT, TRUNCATE ON public.schema_migrations TO %I', runner);
    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;

------------------------------------------------------------
-- 3. Lock down the schema. PUBLIC gets nothing implicit from here on. Must
--    be the LAST statement -- see the header.
------------------------------------------------------------
REVOKE ALL ON SCHEMA public FROM PUBLIC;
