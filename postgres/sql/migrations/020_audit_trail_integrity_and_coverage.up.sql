-- Deep audit 2026-09-02, D-M3 / D-M4 / D-m10. Three findings about the same
-- layer: the forensic trail 006 introduced answers "who changed the rule that
-- decides where money goes" for four tables, records the answer in a place
-- the suspect can write, and misses the one table that is itself the
-- withdrawal gate.
--
-- ####  1. D-M4: the audit tables are writable by the role they audit  ####
--
-- 006 granted ledger_app INSERT on config_table_changes and
-- reconcile_scan_cursor_changes because its trigger functions run with
-- invoker rights, so the trigger's INSERT needed the invoker's grant. That
-- also handed the credential the whole threat model assumes is leaked the
-- ability to write the record of its own actions. Measured, as ledger_app,
-- against a clean install of 001-015:
--
--     INSERT INTO config_table_changes
--       (table_name, old_row, new_row, changed_by, changed_at)
--     VALUES ('currencies','{}','{}','ledger_owner', now() - interval '30 days');
--     -- succeeded; read back as changed_by=ledger_owner, changed_at 30 days ago
--
-- UPDATE and DELETE were already refused, so real rows cannot be erased --
-- but an append-only table an attacker can append to answers "who" with a
-- value the attacker chose, and can be flooded until filtering by table or
-- time is useless. 006's header noticed that current_user does not identify a
-- business actor; it did not notice that DEFAULT current_user is only a
-- default, and a caller naming the column overrides it.
--
-- Fix: the two trigger functions become SECURITY DEFINER and ledger_app's
-- INSERT on both tables, and USAGE on both sequences, is revoked. The trigger
-- path keeps working because it now runs as the owner; the direct path stops
-- existing.
--
-- SECURITY DEFINER is only an improvement if the definer is the right role.
-- Until 019/021 these functions belonged to whichever credential ran migration
-- 006 -- the bootstrap role, which 001 records as holding a permanent ADMIN
-- OPTION on ledger_owner -- so making them SECURITY DEFINER on its own would
-- have promoted an audit INSERT to near-superuser authority. That is why this
-- migration ships in the same batch as 021's ownership sweep and not before
-- it; a run that stops in between leaves golang-migrate's schema_migrations
-- dirty, which is loud, rather than a quietly over-privileged trigger.
--
-- changed_by moves from the column DEFAULT to an explicit `session_user` in
-- the function body, and that is not cosmetic: inside a SECURITY DEFINER
-- function current_user IS the owner, so leaving it to the default would have
-- stamped every audit row 'ledger_owner' and quietly destroyed the very
-- attribution this section is about. session_user is also the stronger
-- choice: it is the role that authenticated, and unlike current_user it
-- cannot be moved by SET ROLE.
--
-- ####  2. D-M3 + D-m10: audit coverage was four hand-picked tables  ####
--
-- 006 attached the audit trigger to currencies, classifications,
-- journal_types and entry_templates -- a list, written by hand, in a schema
-- whose own baseline argues at length that lists are the bug and the
-- catalogue is the fix (001, section 14, deriving ledger_app's ACL from each
-- table's trigger). The same file then derived nothing here.
--
-- What the catalogue says instead: a table carrying a BEFORE UPDATE row
-- trigger that is NOT the blanket ledger_block_mutation() refusal is a table
-- with a *partial* guard -- some updates are meant to get through, and by
-- construction the guard leaves no record of the ones that do. That is the
-- exact population that needs an audit trigger, and it is nine tables, not
-- four. The predicate is 001 section 14's, run in the opposite direction:
-- 001 asks "is this table blanket-guarded?" to decide whether to withhold
-- UPDATE; this asks "is it only partly guarded?" to decide whether to record
-- what got through.
--
-- account_policies is why this is a Major and not tidiness. It is the only
-- DB-enforced freeze/overdraft floor (postgres/account_policy_enforce.go),
-- 006 gave it a guard whose whitelist necessarily contains status,
-- min_balance and enforce_min_balance -- the three enforcement knobs
-- themselves, because UpsertAccountPolicy has to write them -- and then
-- excluded it from the audit triggers on the grounds that it "is the only
-- table in this family with an application-level audit trail
-- (account_policy_changes)". That trail is written by the application, in the
-- same transaction, by the code path an attacker with raw SQL does not use.
-- Measured, as ledger_app:
--
--     UPDATE account_policies SET status='active', min_balance=-1000000,
--            enforce_min_balance=false WHERE account_holder=42;
--     -- succeeded; account_policy_changes: 0 rows; config_table_changes: 0 rows
--
-- Frozen account unfrozen, overdraft floor moved to -1,000,000, and nothing
-- anywhere recorded it. The guard cannot be tightened without breaking
-- SetPolicy, so the answer is the second layer: after this migration that
-- statement still succeeds and now lands a row in config_table_changes
-- carrying the full before/after of all three columns.
--
-- ####  3. Write amplification  ####
--
-- config_table_changes stores to_jsonb(OLD) and to_jsonb(NEW) in full, so an
-- audited UPDATE costs roughly two extra copies of the row. For the config
-- tables that is nothing. For the newly covered ones it is a real capacity
-- change worth stating: journals is updated only by the event_id set-once
-- backfill, deposit_addresses' registration upsert changes nothing and so
-- trips the WHEN clause not at all, entry_template_lines has no legitimate
-- UPDATE left at all -- but bookings and reservations are updated at business
-- rate (every transition, every settle/release). That is the rate at which
-- money moves, and an audit trail that skipped it would be missing the part
-- worth having; docs/CAPACITY.md should size for it.
--
-- events is the one table where the volume is not business rate: its delivery
-- bookkeeping columns are rewritten on every poll and every attempt, which is
-- unbounded in retries and has nothing to do with the ledger's rules. Those
-- columns are subtracted inside the WHEN clause, so an event still leaves an
-- audit row when its journal_id, to_status or amount changes -- 006 lists
-- exactly those as the ones that were writable and must not be -- and leaves
-- none when a delivery attempt ticks. The carve-out is a named exception with
-- a reason, and postgres/audit_trigger_coverage_test.go enumerates the
-- partial-guard population from pg_trigger so a tenth table cannot join it
-- unnoticed.

------------------------------------------------------------
-- 1. Audit writes become owner-only.
------------------------------------------------------------

CREATE OR REPLACE FUNCTION ledger_log_config_table_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (TG_TABLE_NAME, to_jsonb(OLD), to_jsonb(NEW), session_user);
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty,
        new_after_holder, new_after_currency, new_lap_dirty, changed_by
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty, session_user
    );
    RETURN NEW;
END;
$$;

REVOKE INSERT ON public.config_table_changes FROM ledger_app;
REVOKE INSERT ON public.reconcile_scan_cursor_changes FROM ledger_app;
REVOKE USAGE, SELECT ON SEQUENCE public.config_table_changes_id_seq FROM ledger_app;
REVOKE USAGE, SELECT ON SEQUENCE public.reconcile_scan_cursor_changes_id_seq FROM ledger_app;

-- account_policy_changes is the third forensic table and the one 006 pointed
-- at as the reason account_policies needed no DB-level audit. It is written
-- by the application (UpsertAccountPolicy, in the caller's transaction, with
-- a business actor_id the trigger path cannot produce), so its INSERT grant
-- has to stay -- taking it away would delete the only actor attribution the
-- ledger has. Recorded here so the asymmetry with the two tables above reads
-- as a decision and not an omission: that trail answers "which operator", the
-- config_table_changes row this migration now also writes for the same
-- statement answers "which database role", and only the second one is
-- unforgeable by whoever holds the app credential.

------------------------------------------------------------
-- 2. Audit coverage derived from the guard catalogue.
------------------------------------------------------------
DO $$
DECLARE
    r RECORD;
    -- Columns subtracted from the change comparison, per table. Empty for
    -- everything except events -- see the header. Written as a lookup rather
    -- than per-table trigger definitions so the loop below stays derived.
    churn text[];
BEGIN
    FOR r IN
        SELECT DISTINCT c.relname AS table_name
        FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        JOIN pg_proc p ON p.oid = t.tgfoid
        WHERE n.nspname = 'public'
          AND NOT t.tgisinternal
          -- BEFORE (bit 1 set) UPDATE (bit 16) row-level (bit 0) trigger.
          AND (t.tgtype & 2) <> 0
          AND (t.tgtype & 16) <> 0
          AND (t.tgtype & 1) <> 0
          AND p.proname <> 'ledger_block_mutation'
          -- Already carries an audit trigger (the four 006 attached by hand).
          AND NOT EXISTS (
              SELECT 1
              FROM pg_trigger t2
              JOIN pg_proc p2 ON p2.oid = t2.tgfoid
              WHERE t2.tgrelid = c.oid
                AND NOT t2.tgisinternal
                AND p2.proname = 'ledger_log_config_table_change'
          )
        ORDER BY 1
    LOOP
        churn := CASE r.table_name
            WHEN 'events' THEN ARRAY['delivery_status', 'attempts', 'next_attempt_at', 'delivered_at']
            ELSE ARRAY[]::text[]
        END;

        EXECUTE format($fmt$
            CREATE TRIGGER %I
                AFTER UPDATE ON public.%I
                FOR EACH ROW
                WHEN ((to_jsonb(OLD) - %L::text[]) IS DISTINCT FROM (to_jsonb(NEW) - %L::text[]))
                EXECUTE FUNCTION ledger_log_config_table_change()
        $fmt$, r.table_name || '_audit', r.table_name, churn, churn);
    END LOOP;
END $$;

-- No ownership sweep here: 021 runs it once, as the last statement of this
-- batch. See 019's header for why it cannot run before the migrations that
-- still need to replace 001-018's functions and grants.
