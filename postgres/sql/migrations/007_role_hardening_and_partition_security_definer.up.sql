-- ⚠️⚠️  THIS FILE WAS DELIBERATELY MODIFIED AFTER IT HAD BEEN MERGED AND
-- ⚠️⚠️  SHIPPED. Date: 2026-09-02. Finding: 2026-09-02 deep audit, D-M2.
-- ⚠️⚠️  Sanctioned in docs/plans/2026-09-02-remediation-contracts.md §8 as an
-- ⚠️⚠️  explicit exception to §3's "an already-merged migration is immutable"
-- ⚠️⚠️  (and to deployment.md's rule of the same shape).
--
-- WHY an exception was the only option, in three parts:
--
--   1. The bug is that section 1 below could not be executed at all by the
--      bootstrap credential docs/RUNBOOK.md sanctions -- a CREATEROLE,
--      non-superuser role. The install died HERE, in this file, with SQLSTATE
--      42501, and golang-migrate marked the database dirty at 007, so 008
--      onward silently never ran. A later migration cannot reach back and
--      repair a failure point inside an earlier one: the chain never gets far
--      enough to run it.
--   2. golang-migrate does not checksum migration files. A database that has
--      already applied 007 will not re-run it and is not affected by this
--      edit in any way. What changes is only what a FRESH install does.
--   3. This library has no external consumers yet (no-compat window), so
--      there is no third party holding the old file.
--
-- WHAT was changed, exhaustively: section 1's three unconditional
-- `ALTER ROLE ... NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
-- NOBYPASSRLS` statements became one DO block that issues an ALTER only for an
-- attribute a role actually holds, and raises if it cannot strip one. Same
-- intent, same end state, fail-closed instead of the previous silent repair.
-- Nothing else in this file was touched -- not section 2 (the ledger_ro secret
-- revoke), not section 3 (the SECURITY DEFINER partition functions), not a
-- grant, not a comment outside section 1. See the AMENDED note in section 1
-- for the per-clause measurements behind it.
--
-- This is the second time this repository has made this exception (the first
-- was 2026-08-26, "do not add IF NOT EXISTS"). Both times the global rule's
-- literal wording assumed facts -- checksummed migrations, external consumers
-- -- that are not this repository's.
--
-- ------------------------------------------------------------------------
--
-- Three unrelated findings from the same threat-model report, grouped here
-- because each one is a role/grant change and none touches application
-- tables: (1) role attributes silently inherited from a pre-existing role of
-- the same name on a shared cluster, (2) ledger_ro reading the outbound
-- webhook HMAC secret, (3) partition maintenance requiring the serving pool
-- to hold ledger_owner.

-- ####  1. Minor: CREATE ROLE IF NOT EXISTS trusts whatever the cluster
-- ####  already has under these names  ####
--
-- Role names are cluster-global. 001_baseline's `CREATE ROLE IF NOT EXISTS`
-- (really `IF NOT EXISTS (SELECT ...) THEN CREATE ROLE`) only sets
-- attributes on a role it creates; installing onto a cluster that already
-- has a `ledger_app` from a prior install, another tenant, or a manual grant
-- leaves whatever attributes that role already had -- including SUPERUSER or
-- CREATEROLE -- with no warning. I-22 ("ledger_app has no DDL") and the
-- ownership-vs-grant split in section 14 both assume the attributes below;
-- this makes that assumption true unconditionally instead of "true if the
-- role happened to be created by this file". LOGIN is deliberately left
-- alone: all three roles need it for their documented workflows (ledger_app
-- and ledger_ro for serving connections, ledger_owner for the migration job
-- docs/RUNBOOK.md:510 runs directly against it), so this does not touch it.
--
-- ⚠️ AMENDED 2026-09-02 (deep audit D-M2). This section originally issued
-- three unconditional statements:
--
--     ALTER ROLE ledger_owner NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
--     ALTER ROLE ledger_app   ... (same)
--     ALTER ROLE ledger_ro    ... (same)
--
-- and no bootstrap credential short of a full SUPERUSER could run them.
-- Postgres gates each role attribute on the *altering* role holding that
-- same attribute, and it makes the check on whether the clause was written
-- at all -- not on whether it changes anything. Measured on postgres:17.10,
-- as a `CREATEROLE CREATEDB` role holding ADMIN OPTION on the target, every
-- clause issued against a role that already had the attribute cleared:
--
--     NOSUPERUSER   ERROR  Only roles with the SUPERUSER attribute may change ...
--     NOCREATEDB    ok
--     NOCREATEROLE  ok
--     NOREPLICATION ERROR  Only roles with the REPLICATION attribute may change ...
--     NOBYPASSRLS   ERROR  Only roles with the BYPASSRLS attribute may change ...
--
-- and as a CREATEROLE-only role (no CREATEDB), `NOCREATEDB` fails too. So
-- three of the five clauses -- four for the narrowest bootstrap -- are
-- unreachable for the credential docs/RUNBOOK.md sanctions ("superuser, or a
-- role with the CREATEROLE attribute"), which is the standard shape on RDS,
-- Cloud SQL, Neon and Supabase. Such an install died here with SQLSTATE
-- 42501, golang-migrate marked the database dirty at 007, and 008 onward
-- never ran: the ledger_ro secret revoke below, 008's journal_entries.id
-- column-level narrowing and 014's webhook_subscribers write narrowing were
-- all silently absent from exactly the deployments that followed the runbook.
--
-- Editing a migration that is already merged is otherwise forbidden
-- (deployment.md). It is the only option here: the failure is inside 007, so
-- no later migration can reach past it. golang-migrate does not checksum
-- migration files, so a database that already applied 007 will not re-run it
-- and is unaffected -- this changes only what a fresh install does.
--
-- The replacement issues an ALTER only for an attribute a role actually
-- holds. On a clean install (001 just created all three with none of them)
-- that is zero statements, so any bootstrap credential can run it. On the
-- shared cluster this section exists for, the attribute is really set and
-- really has to go: the ALTER is attempted, and if the bootstrap lacks the
-- authority to strip it the install stops with an actionable message instead
-- of continuing on a ledger_app that is SUPERUSER. That is strictly stronger
-- than what this file used to do -- the original blanket ALTER, when it did
-- work, silently repaired the one situation an operator most needs told
-- about -- and it is fail-closed in the sense working-agreements §3 asks for.
--
-- The attribute list is the complete set of role-level privilege attributes
-- Postgres 17 exposes on pg_authid. It is a hardcoded list, which this
-- schema otherwise avoids (see section 14 on deriving from the catalogue),
-- because there is no catalogue view of "attributes that grant privilege"
-- to derive from: pg_roles also carries rolinherit/rolcanlogin/
-- rolconnlimit/rolvaliduntil, which are configuration, not privilege.
-- postgres/roles_test.go asserts all five are false on all three roles, so a
-- future Postgres attribute that is left out of this list is caught there
-- rather than here.
-- ####  EDITED 2026-09-04 (m1 of the 2026-09-03 independent review)  ####
--
-- Deliberate edit to an already-merged migration, under the exception this
-- repository has now taken three times (`docs/plans/2026-09-02-remediation-
-- contracts.md` §3 erratum, which itself records the 2026-08-26 precedent):
-- golang-migrate verifies no file hash, a database that already ran 007 does
-- not re-run it, the failure point is inside 007 so no later migration can
-- reach it, and there is no external consumer. Scope of the edit: the wording
-- of one RAISE. No statement, no condition and no privilege changes.
--
-- What was wrong: the diagnostic named `clauses[i]` -- the clause that CLEARS
-- the attribute -- where it meant to name the attribute the role HOLDS. On a
-- cluster where ledger_app was already SUPERUSER the install stopped with
--
--   ledger: role ledger_app already exists on this cluster with the
--   NOSUPERUSER attribute and this migration credential cannot remove it.
--
-- which reads as the exact opposite of the situation. The remedy half of the
-- sentence was right all along; only the diagnostic half was inverted. For a
-- message whose entire value is being actionable at install time, that is
-- worth one array.
DO $$
DECLARE
    -- pg_authid column, the attribute name a human recognises, and the ALTER
    -- ROLE clause that clears it. Three parallel arrays rather than two: the
    -- attribute a role HOLDS and the clause that REMOVES it are different
    -- words, and printing one where the other belongs is m1.
    attrs   CONSTANT text[] := ARRAY['rolsuper',    'rolcreatedb', 'rolcreaterole', 'rolreplication', 'rolbypassrls'];
    held_as CONSTANT text[] := ARRAY['SUPERUSER',   'CREATEDB',    'CREATEROLE',    'REPLICATION',    'BYPASSRLS'];
    clauses CONSTANT text[] := ARRAY['NOSUPERUSER', 'NOCREATEDB',  'NOCREATEROLE',  'NOREPLICATION',  'NOBYPASSRLS'];
    role_name text;
    i int;
    held boolean;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['ledger_owner', 'ledger_app', 'ledger_ro'] LOOP
        FOR i IN 1 .. array_length(attrs, 1) LOOP
            EXECUTE format('SELECT %I FROM pg_roles WHERE rolname = %L', attrs[i], role_name) INTO held;
            CONTINUE WHEN held IS NOT TRUE;

            BEGIN
                EXECUTE format('ALTER ROLE %I %s', role_name, clauses[i]);
            EXCEPTION WHEN insufficient_privilege THEN
                RAISE EXCEPTION
                    'ledger: role % already exists on this cluster with the % attribute and this migration credential cannot remove it. This install would run on a % that holds a privilege I-22 assumes it does not. Strip it with a superuser connection (ALTER ROLE % %) and re-run the migration, or install into a cluster that does not already own these role names.',
                    role_name, held_as[i], role_name, role_name, clauses[i]
                    USING ERRCODE = 'insufficient_privilege';
            END;
        END LOOP;
    END LOOP;
END $$;

-- ####  2. Major: ledger_ro can read every outbound webhook's HMAC secret  ####
--
-- 001_baseline's `GRANT SELECT ON ALL TABLES IN SCHEMA public TO ledger_ro`
-- was written under the framing "ledger_ro is broader than ideal -- it can
-- read data it should not" (a confidentiality non-goal the design docs
-- already accept). webhook_subscribers.secret is not that: it is the key
-- ledger_ro's own holder uses to authenticate every event this ledger sends
-- outbound (service/delivery/webhook.go). Reading it does not just disclose
-- data, it hands a read-only credential the ability to forge signed event
-- deliveries to any subscriber. Confirmed by connecting as ledger_ro and
-- selecting url, secret straight off the table before this migration.
--
-- Column-level GRANT, not a view: a view would need its own ACL story (who
-- can it be granted to, does it show up in the same audits) and would still
-- have to enumerate every column except secret, which is what this does
-- directly. REVOKE first, because table-level SELECT and column-level SELECT
-- are different ACL entries -- granting a subset of columns without revoking
-- the table-level grant leaves the table-level grant (which does cover
-- secret) still in force.
REVOKE SELECT ON public.webhook_subscribers FROM ledger_ro;
GRANT SELECT (
    id, name, url, filter_class, filter_to_status, is_active,
    created_at, last_status_code, last_error, last_attempt_at
) ON public.webhook_subscribers TO ledger_ro;

-- ####  3. Major: partition maintenance requires ledger_owner, and
-- ####  ledger_owner's TRUNCATE walks straight past the append-only trigger
-- ####  ####
--
-- postgres/partition_store.go issues CREATE TABLE ... PARTITION OF, ALTER
-- TABLE ... DETACH/ATTACH PARTITION and TRUNCATE directly. All four are
-- schema-owner-gated DDL; ledger_app has none of them (confirmed: permission
-- denied for schema public / must be owner of table / permission denied for
-- table, run as ledger_app against each in turn). The only way the shipped
-- worker's partition job has ever run, then, is a serving pool connected as
-- ledger_owner -- which also means that pool's TRUNCATE bypasses
-- journal_entries' no-DELETE trigger entirely (TRUNCATE does not fire
-- row-level triggers; confirmed by inserting two real, balanced journal
-- entries into the default partition, connecting as ledger_owner, and
-- watching TRUNCATE journal_entries_default silently remove both with no
-- trigger firing).
--
-- SECURITY DEFINER closes the gap the RUNBOOK gap analysis called out
-- (needing a *second*, owner-backed pool) without granting ledger_app the
-- DDL that would create: these two functions run with their owner's
-- (ledger_owner's) privileges no matter which role calls them, so ledger_app
-- gets EXECUTE and nothing more. Blast radius from a leaked ledger_app
-- credential shrinks from "unconditional TRUNCATE on journal_entries_default
-- at will" to "can call this specific function, which only ever truncates
-- rows it has just copied into their permanent partitions inside the same
-- statement" -- the same move-then-truncate order partition_store.go always
-- used, now enforced as the only path available rather than a convention the
-- Go caller happens to follow.
--
-- SET search_path = public on both: SECURITY DEFINER functions run with the
-- privileges of their owner, so an uncontrolled search_path is a
-- schema-shadowing vector (a caller-writable schema earlier in the path
-- could substitute an object the function resolves unqualified). Pinning it
-- removes that regardless of the caller's own search_path.
CREATE OR REPLACE FUNCTION ledger_create_monthly_partition(
    p_name text, p_from date, p_to date
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    -- p_name is interpolated into DDL via format(%I); constraining its shape
    -- here (rather than trusting the Go caller) is what makes that safe
    -- against a caller that is not partition_store.go, since EXECUTE
    -- privilege on this function is a real ledger_app-reachable capability.
    IF p_name !~ '^journal_entries_y[0-9]{4}m[0-9]{2}$' THEN
        RAISE EXCEPTION 'ledger: invalid monthly partition name %', p_name
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

-- Mirrors postgres/partition_store.go's rebalanceDefault: detach the default
-- partition, create every monthly partition needed to cover both the
-- requested horizon and any rows actually found in the default, move those
-- rows into their monthly homes, truncate what is now guaranteed to be an
-- exact copy already living elsewhere, and re-attach an empty default. All
-- of it runs as one statement from the caller's side, which makes it
-- atomic (a plpgsql function body is one implicit transaction unless the
-- caller wraps it in an explicit one) without partition_store.go having to
-- manage a transaction by hand.
CREATE OR REPLACE FUNCTION ledger_rebalance_default_partition(
    p_first date, p_last date
) RETURNS text[]
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    created    text[] := '{}';
    v_min      timestamptz;
    v_max      timestamptz;
    v_has_rows boolean;
    v_month    date;
    v_name     text;
BEGIN
    ALTER TABLE journal_entries DETACH PARTITION journal_entries_default;

    SELECT min(created_at), max(created_at) INTO v_min, v_max FROM journal_entries_default;
    v_has_rows := v_min IS NOT NULL;

    IF v_has_rows THEN
        IF date_trunc('month', v_min)::date < p_first THEN
            p_first := date_trunc('month', v_min)::date;
        END IF;
        IF date_trunc('month', v_max)::date > p_last THEN
            p_last := date_trunc('month', v_max)::date;
        END IF;
    END IF;

    v_month := p_first;
    WHILE v_month <= p_last LOOP
        v_name := format('journal_entries_y%sm%s', to_char(v_month, 'YYYY'), to_char(v_month, 'MM'));
        IF to_regclass('public.' || v_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
                v_name, v_month, (v_month + INTERVAL '1 month')::date
            );
        END IF;
        created := array_append(created, v_name);
        v_month := (v_month + INTERVAL '1 month')::date;
    END LOOP;

    IF v_has_rows THEN
        INSERT INTO journal_entries SELECT * FROM journal_entries_default;
        TRUNCATE journal_entries_default;
    END IF;

    ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT;

    RETURN created;
END;
$$;

REVOKE ALL ON FUNCTION ledger_rebalance_default_partition(date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_rebalance_default_partition(date, date) TO ledger_app;
