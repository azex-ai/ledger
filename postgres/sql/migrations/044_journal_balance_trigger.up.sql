-- 044_journal_balance_trigger.up.sql
--
-- P3 (integrity hardening, C1): restore per-journal / per-currency balance
-- enforcement at the DB layer.
--
-- Migration 004 first added this as a per-row CONSTRAINT TRIGGER. Each row's
-- firing re-scanned every entry of the affected journal via one query --
-- O(N) work per row, O(N^2) total for a journal of N entries. Migration 018
-- dropped it for exactly that reason and pushed the check into the
-- application layer only (postgres/ledger_store.go's VerifyJournalBalanced,
-- one query per posted journal, run inside the same transaction as the
-- inserts). That left a real gap (C1,
-- docs/plans/2026-08-21-tamper-evident-ledger-design.md §2): a direct SQL
-- INSERT into journal_entries -- bypassing the application entirely, e.g. an
-- attacker with a leaked app DB credential -- can post unbalanced entries
-- with nothing in the DB to stop it.
--
-- This migration restores DB-layer enforcement while staying O(N) overall.
-- PostgreSQL constraint triggers must be FOR EACH ROW (statement-level
-- constraint triggers do not exist), so the row-level firing itself cannot be
-- avoided. Instead, the trigger function dedupes by journal_id within the
-- current transaction using a transaction-scoped temp table
-- (ON COMMIT DELETE ROWS, so no state leaks across transactions on a pooled
-- connection): the first row belonging to a given journal_id in this
-- transaction runs the aggregate balance check for that journal; every
-- subsequent row for the same journal_id in the same transaction (whether
-- from the same INSERT statement or a later one -- the check reads the base
-- table, not the triggering row, so statement boundaries don't matter) is a
-- cheap dedup no-op. The result is exactly one aggregate query per journal
-- touched by the transaction, not one per row -- matching the design's "restore
-- the trigger, but per-journal one aggregate query, not 004's per-row O(N^2)".
--
-- Application-layer VerifyJournalBalanced is unchanged and still runs first
-- (in the same transaction, with a better error message pinpointing the
-- offending currency). This trigger is the DB-layer backstop for direct SQL
-- or a compromised app credential that bypasses the application layer.

CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER AS $$
DECLARE
    target_journal_id BIGINT;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    -- Transaction-scoped dedup set, lives in pg_temp, cleared automatically
    -- at the end of every transaction. INSERT ... ON CONFLICT DO NOTHING
    -- sets FOUND=true only when this journal_id was NOT already present
    -- (i.e. this is the first row for it in the current transaction).
    CREATE TEMP TABLE IF NOT EXISTS ledger_balance_checked (
        journal_id BIGINT PRIMARY KEY
    ) ON COMMIT DELETE ROWS;

    INSERT INTO ledger_balance_checked (journal_id)
    VALUES (target_journal_id)
    ON CONFLICT DO NOTHING;

    IF NOT FOUND THEN
        -- Already validated this journal_id earlier in this transaction.
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM journal_entries
        WHERE journal_id = target_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END
        ) <> 0
    ) THEN
        RAISE EXCEPTION 'journal % has unbalanced entries by currency', target_journal_id
            USING
                ERRCODE = '23514',
                CONSTRAINT = 'chk_journal_currency_balance';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_check_journal_currency_balance ON journal_entries;
CREATE CONSTRAINT TRIGGER trg_check_journal_currency_balance
    AFTER INSERT OR UPDATE OR DELETE ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION check_journal_currency_balance();

-- Note: this trigger validates only rows written AFTER this migration runs
-- (constraint triggers are not retroactively applied to pre-existing rows).
-- Any journal that became unbalanced during the window between migration
-- 018 (trigger dropped) and this one (trigger restored) is NOT caught by
-- this trigger. The fleet-wide "journal_dr_cr" reconcile check (M1 fix, same
-- design doc §5) re-verifies every journal in bulk for exactly that reason.
