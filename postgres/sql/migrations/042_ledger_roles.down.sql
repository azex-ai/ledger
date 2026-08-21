-- 042_ledger_roles.down.sql
--
-- Reverses 042_ledger_roles.up.sql: reassigns ownership back to whoever
-- runs this rollback, revokes every grant made to the three roles, and
-- drops them. Assumes it runs under the same identity that owned these
-- objects before up.sql ran -- true as long as DATABASE_URL has not been
-- cut over to ledger_owner/ledger_app (the "migrate" phase this migration
-- deliberately does not perform; see up.sql header).

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
    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO %I', r.sequencename, runner);
    END LOOP;

    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;

------------------------------------------------------------
-- 2. Undo the default-privilege entry that benefited ledger_owner.
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON TABLES FROM ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ledger_owner', runner);
END $$;

------------------------------------------------------------
-- 3. Revoke every table/sequence grant made to ledger_app and ledger_ro.
------------------------------------------------------------
REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM ledger_ro;
REVOKE SELECT ON ALL SEQUENCES IN SCHEMA public FROM ledger_ro;

DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
    LOOP
        EXECUTE format('REVOKE ALL ON public.%I FROM ledger_app', r.relname);
    END LOOP;
    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('REVOKE ALL ON public.%I FROM ledger_app', r.sequencename);
    END LOOP;
END $$;

------------------------------------------------------------
-- 4. Undo the schema-level lockdown. Not restored: PUBLIC's pre-042
--    schema-level privileges beyond USAGE -- modern Postgres (15+) does not
--    grant CREATE on the public schema to PUBLIC by default either, so
--    restoring exactly the pre-042 ACL is not a meaningful safety property
--    to chase here.
------------------------------------------------------------
REVOKE CREATE ON SCHEMA public FROM ledger_owner;
REVOKE USAGE ON SCHEMA public FROM ledger_owner, ledger_app, ledger_ro;
GRANT USAGE ON SCHEMA public TO PUBLIC;

------------------------------------------------------------
-- 5. Safety net: drop anything the three roles still own or hold anywhere
--    (should be nothing after steps 1-3), then drop the roles.
------------------------------------------------------------
DROP OWNED BY ledger_owner, ledger_app, ledger_ro;

DROP ROLE IF EXISTS ledger_owner;
DROP ROLE IF EXISTS ledger_app;
DROP ROLE IF EXISTS ledger_ro;
