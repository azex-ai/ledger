-- Deep audit 2026-09-02, D-M1: 001_baseline's ownership sweep is a one-shot
-- loop at the bottom of one file, so it only ever covered what 001 itself
-- created. Every object built by 002 onward is still owned by whichever
-- credential ran the migration.
--
-- 001 (section "Ownership") wrote the failure mode down verbatim before
-- shipping it:
--
--     "a table created by a migration that ran AFTER the ownership sweep
--      never got swept ... Sweeping the catalogue instead of a list of names
--      is what makes both classes impossible here"
--
-- Sweeping the catalogue made it impossible *within 001*. Nothing made it
-- impossible across files, and nothing looked: a full-repo grep for relowner
-- / proowner / OWNER TO returned only 001's own up/down pair. Measured on a
-- clean install of 001-015: 4 tables, 4 sequences and 9 functions came back
-- owned by the bootstrap credential.
--
-- Two of those nine functions are ledger_create_monthly_partition and
-- ledger_rebalance_default_partition -- SECURITY DEFINER, with EXECUTE
-- granted to ledger_app. A SECURITY DEFINER function runs with its owner's
-- privileges, so the credential the whole threat model assumes is leaked held
-- two entry points running as the *bootstrap* role rather than as
-- ledger_owner. 007's own header argues its blast radius shrinks because
-- these run as ledger_owner; that premise was false in every deployment.
-- I-35 states it as fact ("Both functions are owned by ledger_owner"), which
-- is why this migration and that invariant's rewording land together.
--
-- The other seven are the guard and audit trigger functions from 003/006/010.
-- 001 explains why their owner matters better than this comment can: an owner
-- can CREATE OR REPLACE the body of ledger_block_mutation with `BEGIN RETURN
-- NEW; END` and every append-only guarantee in the schema turns off, quietly,
-- leaving all the triggers in place and doing nothing. 001 also records that
-- the bootstrap credential keeps a permanent ADMIN OPTION on ledger_owner and
-- should therefore be rotated or retired after install -- which DROP ROLE
-- refuses to do while it still owns objects. This migration is also what
-- makes that advice executable.
--
-- ####  Why a function, and not another inline DO block  ####
--
-- Because the gap was structural, not a missing statement. A copy of 001's
-- loop pasted here would fix 002-018 and leave 020 exposed the same way 002
-- was. Extracting it makes "put ownership back where it belongs" a callable,
-- idempotent step every future migration ends with, and
-- postgres/object_ownership_test.go turns the rule into a gate: it enumerates
-- every relation and every routine in `public` and fails on anything not
-- owned by ledger_owner, so a migration that forgets the call goes red on the
-- PR that adds it.
--
-- Idempotent by skipping objects already owned by ledger_owner, not by
-- re-issuing a no-op ALTER. That is not a micro-optimization: the migration
-- credential holds SET but deliberately not INHERIT on ledger_owner, so once
-- an object's owner IS ledger_owner the credential no longer passes
-- Postgres's ownership check for it and the ALTER fails outright. 001's
-- sequence loop already carries this filter and its comment explains why.

CREATE OR REPLACE FUNCTION ledger_resweep_ownership() RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    r         RECORD;
    swept     integer := 0;
BEGIN
    -- Ordinary tables, partitioned tables and partitions. Partitions are
    -- included deliberately: ALTER TABLE ... OWNER TO does not recurse into
    -- them, and ledger_rebalance_default_partition creates them at runtime.
    -- (Once this migration has run, that function is itself owned by
    -- ledger_owner and, being SECURITY DEFINER, creates partitions already
    -- owned by ledger_owner -- so this loop is the backstop for partitions
    -- created before today, not the ongoing mechanism.)
    --
    -- schema_migrations is the one name this sweep skips. golang-migrate
    -- creates it as the migration runner, and 001 transfers it and then
    -- re-grants the runner SELECT/INSERT/TRUNCATE through a temporary
    -- INHERIT upgrade of its ledger_owner membership -- a manoeuvre only the
    -- credential that created the roles can perform. Re-transferring it here
    -- without that re-grant would rewrite the ACL entry the running migration
    -- is standing on. It is already ledger_owner's after 001, so this loop
    -- has nothing to do for it anyway; object_ownership_test.go asserts that
    -- rather than taking it on faith.
    FOR r IN
        SELECT c.relname, c.relkind
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('r', 'p')
          AND c.relname <> 'schema_migrations'
          AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
    LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO ledger_owner', r.relname);
        swept := swept + 1;
    END LOOP;

    -- After the table loop: a table's owner change carries its owned
    -- (BIGSERIAL-backed) sequences with it, so re-reading the catalogue here
    -- leaves only sequences that are genuinely standalone.
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind = 'S'
          AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
    LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ledger_owner', r.relname);
        swept := swept + 1;
    END LOOP;

    FOR r IN
        SELECT c.relname, c.relkind
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('v', 'm')
          AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
    LOOP
        IF r.relkind = 'v' THEN
            EXECUTE format('ALTER VIEW public.%I OWNER TO ledger_owner', r.relname);
        ELSE
            EXECUTE format('ALTER MATERIALIZED VIEW public.%I OWNER TO ledger_owner', r.relname);
        END IF;
        swept := swept + 1;
    END LOOP;

    -- Functions and procedures, this function included. Altering the owner of
    -- a routine that is mid-execution is permitted -- the plan in flight is
    -- already resolved -- and including it is the point: the sweep must not
    -- need a companion statement to finish the job on itself, or the next
    -- person to add a helper here reproduces exactly the omission this
    -- migration exists to close.
    FOR r IN
        SELECT p.oid::regprocedure AS sig
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND p.prokind IN ('f', 'p')
          AND pg_get_userbyid(p.proowner) <> 'ledger_owner'
    LOOP
        EXECUTE format('ALTER ROUTINE %s OWNER TO ledger_owner', r.sig);
        swept := swept + 1;
    END LOOP;

    RETURN swept;
END;
$$;

-- Not a capability ledger_app has any use for. A new function is EXECUTE-able
-- by PUBLIC unless told otherwise, and this schema's role separation is not
-- something a default should decide -- see 021, which closes that default for
-- every function at once and gates it.
REVOKE ALL ON FUNCTION ledger_resweep_ownership() FROM PUBLIC;

-- ####  Why this file does not call the function it defines  ####
--
-- Because the sweep has to run after the last object in this batch is
-- created, and 020 and 021 both create some. Anything it transferred before
-- them would be swept, and then their new functions and grants would not be --
-- which is the exact gap D-M1 is about, reintroduced one batch later.
--
-- (An earlier draft of this comment gave a different reason: that ownership
-- transfer is a one-way door mid-run, because the migration credential holds
-- SET but not INHERIT on ledger_owner and so stops passing the ownership
-- check for anything it hands over. That was true before postgres.Migrate
-- took ledger_owner's privileges for each migration it applies. It is not any
-- more, and leaving it written down would have had the next author solving a
-- problem that no longer exists.)
--
-- So the sweep runs exactly once, as the last statement of the last migration
-- in this batch (021). The rule that replaces "remember to call it" is
-- postgres/object_ownership_test.go, which fails on any relation or routine
-- in `public` not owned by ledger_owner after all migrations have run.
--
-- A future migration that must modify an object this sweep has already
-- transferred needs nothing: postgres.Migrate holds ledger_owner's privileges
-- for the duration of each migration it applies (see applyRemainingMigrations
-- in postgres/migrate.go) and hands them back afterwards. Just write the
-- statement.
--
-- ⚠️ Do NOT reach for 001's "Keepsake 2 of 2" manoeuvre --
--
--     DO $$ DECLARE runner text := current_user; BEGIN
--         EXECUTE format('GRANT ledger_owner TO %I WITH INHERIT TRUE', runner);
--     END $$;
--     -- ... statements needing ledger_owner's authority ...
--     DO $$ DECLARE runner text := current_user; BEGIN
--         EXECUTE format('REVOKE ledger_owner FROM %I', runner);
--     END $$;
--
-- -- inside a migration after 001. 001 has to do it that way because it runs
-- before the mechanism exists; everything after 001 does not, and the REVOKE
-- half is actively harmful: the runner is the only role that can issue either
-- grant, so both carry the same grantor and Postgres has one row to revoke.
-- Revoking "your own" membership therefore ends Migrate's window as well.
-- Measured (2026-09-03): 018 uses this idiom, and under Migrate's original
-- single run-wide window it left 019, 020 and 021 unprivileged -- a CREATEROLE
-- bootstrap install died at 020's CREATE TRIGGER with "permission denied for
-- table account_policies", dirty at 20. Migrate now opens one window per
-- migration precisely so that a migration cannot do this to its successors,
-- but the idiom is still dead weight and still misleading, so: don't.
--
-- What a migration creating new objects DOES still owe is a call to
--
--     SELECT ledger_resweep_ownership();
--
-- as its last statement, because objects created while holding that membership
-- belong to the runner, not to ledger_owner.
