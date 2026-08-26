-- Closes M-1 (`.local/independent-review-2026-08-26.md`,
-- docs/plans/2026-08-26-audit-remediation-contracts.md follow-on fix-backend-1
-- batch, board #43): check #2's `Complete=true` signal only ever distrusted a
-- resumed lap that found LITERALLY ZERO pairs on its first page
-- (`service/reconcile.go`'s `scanned == 0 && resumedLap` branch, migration
-- 006's threat-model.md finding). A cursor tampered to leave exactly one (or
-- any small number of) pairs unscanned sailed straight through that check:
-- the run scanned its one remaining pair, found nothing wrong, and reported
-- Complete=true -- resetting the cursor back to the fresh sentinel and
-- discarding every real pair the tampering skipped. One forged
-- `UPDATE reconcile_scan_cursors SET after_holder = <second-to-last>` bought
-- one permanently clean-looking run, repeatable indefinitely.
--
-- The Go-side fix (service/reconcile.go, same task) needs an independent
-- signal that a resumed run's own starting cursor cannot influence: how many
-- pairs has this LAP verified in total across every run since it last
-- started fresh, compared against how many pairs the checkpoint fleet
-- actually has right now. lap_scanned is that cumulative counter --
-- persisted alongside the existing resume cursor, reset to 0 exactly when
-- the cursor resets to the fresh-lap sentinel, and otherwise carrying
-- forward lapScannedAtStart + each run's own genuinely-queried page results.
--
-- Same caveat migration 006 already wrote down for this table, unchanged by
-- this migration: reconcile_scan_cursors has no DB-level mutation guard, and
-- cannot practically have one -- SetScanCursor legitimately overwrites every
-- column here to any value, including the fresh-lap reset. A leaked
-- ledger_app credential that forges after_holder AND a matching lap_scanned
-- in the same statement still defeats this check; what this closes is the
-- much cheaper attack that only had to touch after_holder. The AFTER UPDATE
-- audit trigger 006 installed is extended below to also log lap_scanned
-- changes, so that harder attack still leaves a trace even though the Go
-- layer alone cannot refuse it (same "detection, not prevention" stance 006
-- documents for this table).
ALTER TABLE reconcile_scan_cursors
    ADD COLUMN lap_scanned BIGINT NOT NULL DEFAULT 0;

ALTER TABLE reconcile_scan_cursor_changes
    ADD COLUMN old_lap_scanned BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN new_lap_scanned BIGINT NOT NULL DEFAULT 0;

CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty, old_lap_scanned,
        new_after_holder, new_after_currency, new_lap_dirty, new_lap_scanned
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty, OLD.lap_scanned,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty, NEW.lap_scanned
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS reconcile_scan_cursors_audit ON reconcile_scan_cursors;

CREATE TRIGGER reconcile_scan_cursors_audit
    AFTER UPDATE ON reconcile_scan_cursors
    FOR EACH ROW
    WHEN (OLD.after_holder IS DISTINCT FROM NEW.after_holder
       OR OLD.after_currency IS DISTINCT FROM NEW.after_currency
       OR OLD.lap_dirty IS DISTINCT FROM NEW.lap_dirty
       OR OLD.lap_scanned IS DISTINCT FROM NEW.lap_scanned)
    EXECUTE FUNCTION ledger_log_reconcile_scan_cursor_change();
