-- 018_restore_referential_integrity.down.sql
-- Reverse migration 018.

-- 10. Drop rollup_queue.failed_attempts column.
ALTER TABLE rollup_queue DROP COLUMN IF EXISTS failed_attempts;

-- 9. Restore the per-row constraint trigger (deferred at commit) for journal balance.
CREATE OR REPLACE FUNCTION check_journal_currency_balance()
RETURNS TRIGGER AS $$
DECLARE
    target_journal_id BIGINT;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);

    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM journal_entries
        WHERE journal_id = target_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE
                WHEN entry_type = 'debit' THEN amount
                ELSE -amount
            END
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

CREATE CONSTRAINT TRIGGER trg_check_journal_currency_balance
AFTER INSERT OR UPDATE OR DELETE ON journal_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION check_journal_currency_balance();

-- 8. Drop append-only triggers.
DROP TRIGGER IF EXISTS journals_no_arbitrary_update ON journals;
DROP FUNCTION IF EXISTS ledger_journals_block_arbitrary_update();
DROP TRIGGER IF EXISTS journals_no_delete ON journals;
DROP TRIGGER IF EXISTS journal_entries_no_delete ON journal_entries;
DROP TRIGGER IF EXISTS journal_entries_no_update ON journal_entries;
DROP FUNCTION IF EXISTS ledger_block_mutation();

-- 7. JSONB metadata: revert NOT NULL on deposits/withdrawals.
ALTER TABLE withdrawals ALTER COLUMN metadata DROP NOT NULL;
ALTER TABLE deposits ALTER COLUMN metadata DROP NOT NULL;

-- 6. journals.metadata: revert NOT NULL.
ALTER TABLE journals ALTER COLUMN metadata DROP NOT NULL;

-- 5. events.booking_id: drop FK.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_booking_id_fkey;

-- 4. events.journal_id: drop FK, restore NOT NULL DEFAULT 0 sentinel.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_journal_id_fkey;
UPDATE events SET journal_id = 0 WHERE journal_id IS NULL;
ALTER TABLE events ALTER COLUMN journal_id SET DEFAULT 0;
ALTER TABLE events ALTER COLUMN journal_id SET NOT NULL;

-- 3. bookings.classification_id and currency_id: drop FKs.
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_currency_id_fkey;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_classification_id_fkey;

-- 2. bookings.reservation_id: drop FK, restore NOT NULL DEFAULT 0 sentinel.
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_reservation_id_fkey;
UPDATE bookings SET reservation_id = 0 WHERE reservation_id IS NULL;
ALTER TABLE bookings ALTER COLUMN reservation_id SET DEFAULT 0;
ALTER TABLE bookings ALTER COLUMN reservation_id SET NOT NULL;

-- 1. bookings.journal_id: drop FK, restore NOT NULL DEFAULT 0 sentinel.
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_journal_id_fkey;
UPDATE bookings SET journal_id = 0 WHERE journal_id IS NULL;
ALTER TABLE bookings ALTER COLUMN journal_id SET DEFAULT 0;
ALTER TABLE bookings ALTER COLUMN journal_id SET NOT NULL;
