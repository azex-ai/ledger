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
DO $$
DECLARE
    -- pg_authid column, and the ALTER ROLE clause that clears it.
    attrs   CONSTANT text[] := ARRAY['rolsuper',    'rolcreatedb', 'rolcreaterole', 'rolreplication', 'rolbypassrls'];
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
                    role_name, clauses[i], role_name, role_name, clauses[i]
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
