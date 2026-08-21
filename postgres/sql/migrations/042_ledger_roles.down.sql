-- 042_ledger_roles.down.sql
--
-- Reverses 042_ledger_roles.up.sql. Since up.sql is additive only (no
-- REVOKE, no ownership transfer), down.sql is symmetric: undo every GRANT
-- and default-privilege entry, then drop the three roles. No ownership
-- reassignment is needed because none ever happened.

------------------------------------------------------------
-- 1. Undo the default-privilege entry that benefits ledger_owner.
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON TABLES FROM ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ledger_owner', runner);
END $$;

------------------------------------------------------------
-- 2. Revoke every table/sequence grant made to ledger_app and ledger_ro.
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
-- 3. Undo the schema-level grants issued in step 2 of up.sql. PUBLIC's own
--    access was never touched by up.sql, so there is nothing to restore
--    for it here.
------------------------------------------------------------
REVOKE CREATE ON SCHEMA public FROM ledger_owner;
REVOKE USAGE ON SCHEMA public FROM ledger_owner, ledger_app, ledger_ro;

------------------------------------------------------------
-- 4. Safety net: drop anything the three roles still hold anywhere (should
--    be nothing after steps 1-3; none of them ever owned an object), then
--    drop the roles.
------------------------------------------------------------
DROP OWNED BY ledger_owner, ledger_app, ledger_ro;

DROP ROLE IF EXISTS ledger_owner;
DROP ROLE IF EXISTS ledger_app;
DROP ROLE IF EXISTS ledger_ro;
