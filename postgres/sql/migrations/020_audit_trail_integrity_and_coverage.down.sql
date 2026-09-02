-- Reverses the audit trigger coverage and the SECURITY DEFINER change,
-- restoring exactly migration 006's shape.
--
-- ⚠️ Must run while the two trigger functions and the newly audited tables are
-- still owned by the credential issuing this file, or from a superuser
-- connection. See 019's down for the fence and for the SET ROLE shape that
-- gets around it.

------------------------------------------------------------
-- Audit trigger coverage. Derived the same way it was added: every
-- ledger_log_config_table_change() trigger except the four migration 006
-- attached by hand, which this file is rolling back to.
------------------------------------------------------------
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname AS table_name, t.tgname
        FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        JOIN pg_proc p ON p.oid = t.tgfoid
        WHERE n.nspname = 'public'
          AND NOT t.tgisinternal
          AND p.proname = 'ledger_log_config_table_change'
          AND c.relname NOT IN ('currencies', 'classifications', 'journal_types', 'entry_templates')
    LOOP
        EXECUTE format('DROP TRIGGER %I ON public.%I', r.tgname, r.table_name);
    END LOOP;
END $$;

------------------------------------------------------------
-- Invoker rights + the DEFAULT current_user attribution 006 shipped.
------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_log_config_table_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO config_table_changes (table_name, old_row, new_row)
    VALUES (TG_TABLE_NAME, to_jsonb(OLD), to_jsonb(NEW));
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty,
        new_after_holder, new_after_currency, new_lap_dirty
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty
    );
    RETURN NEW;
END;
$$;

-- Invoker rights make the trigger's INSERT the invoker's INSERT again, so the
-- grants have to come back with them or every guarded config UPDATE starts
-- failing on a permission error.
GRANT INSERT ON public.config_table_changes TO ledger_app;
GRANT INSERT ON public.reconcile_scan_cursor_changes TO ledger_app;
GRANT USAGE, SELECT ON SEQUENCE public.config_table_changes_id_seq TO ledger_app;
GRANT USAGE, SELECT ON SEQUENCE public.reconcile_scan_cursor_changes_id_seq TO ledger_app;
