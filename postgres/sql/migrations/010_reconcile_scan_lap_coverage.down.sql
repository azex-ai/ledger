DROP TRIGGER IF EXISTS reconcile_scan_cursors_audit ON reconcile_scan_cursors;

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

CREATE TRIGGER reconcile_scan_cursors_audit
    AFTER UPDATE ON reconcile_scan_cursors
    FOR EACH ROW
    WHEN (OLD.after_holder IS DISTINCT FROM NEW.after_holder
       OR OLD.after_currency IS DISTINCT FROM NEW.after_currency
       OR OLD.lap_dirty IS DISTINCT FROM NEW.lap_dirty)
    EXECUTE FUNCTION ledger_log_reconcile_scan_cursor_change();

ALTER TABLE reconcile_scan_cursor_changes
    DROP COLUMN old_lap_scanned,
    DROP COLUMN new_lap_scanned;

ALTER TABLE reconcile_scan_cursors
    DROP COLUMN lap_scanned;
