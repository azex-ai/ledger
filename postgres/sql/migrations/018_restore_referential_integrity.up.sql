-- 018_restore_referential_integrity.up.sql
--
-- Restore foreign key constraints lost when migration 017 forced these
-- "FK target" columns to NOT NULL with a 0 sentinel. Postgres cannot enforce
-- referential integrity against a non-NULL sentinel, so for *FK target*
-- columns the No-NULL rule is relaxed: a missing relation must be NULL.
--
-- Also: enforce append-only on journal_entries and journals (allow only
-- INSERT and the single allowed reversed_by/event_id update path).
--
-- Also: clean up the remaining nullable JSONB metadata columns missed in 017.

------------------------------------------------------------
-- 1. bookings.journal_id: NOT NULL 0 -> nullable, restore FK
------------------------------------------------------------
ALTER TABLE bookings ALTER COLUMN journal_id DROP DEFAULT;
ALTER TABLE bookings ALTER COLUMN journal_id DROP NOT NULL;
UPDATE bookings SET journal_id = NULL WHERE journal_id = 0;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_journal_id_fkey
    FOREIGN KEY (journal_id) REFERENCES journals(id) ON DELETE RESTRICT;

------------------------------------------------------------
-- 2. bookings.reservation_id: NOT NULL 0 -> nullable, restore FK
------------------------------------------------------------
ALTER TABLE bookings ALTER COLUMN reservation_id DROP DEFAULT;
ALTER TABLE bookings ALTER COLUMN reservation_id DROP NOT NULL;
UPDATE bookings SET reservation_id = NULL WHERE reservation_id = 0;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_reservation_id_fkey
    FOREIGN KEY (reservation_id) REFERENCES reservations(id) ON DELETE RESTRICT;

------------------------------------------------------------
-- 3. bookings.classification_id and currency_id: bare BIGINT -> add FK (still NOT NULL)
------------------------------------------------------------
ALTER TABLE bookings
    ADD CONSTRAINT bookings_classification_id_fkey
    FOREIGN KEY (classification_id) REFERENCES classifications(id) ON DELETE RESTRICT;

ALTER TABLE bookings
    ADD CONSTRAINT bookings_currency_id_fkey
    FOREIGN KEY (currency_id) REFERENCES currencies(id) ON DELETE RESTRICT;

------------------------------------------------------------
-- 4. events.journal_id: NOT NULL 0 -> nullable, restore FK
------------------------------------------------------------
ALTER TABLE events ALTER COLUMN journal_id DROP DEFAULT;
ALTER TABLE events ALTER COLUMN journal_id DROP NOT NULL;
UPDATE events SET journal_id = NULL WHERE journal_id = 0;
ALTER TABLE events
    ADD CONSTRAINT events_journal_id_fkey
    FOREIGN KEY (journal_id) REFERENCES journals(id) ON DELETE RESTRICT;

------------------------------------------------------------
-- 5. events.booking_id: bare BIGINT -> add FK (still NOT NULL)
--    booking_id = 0 is treated as "no booking" historically, but in v2 every
--    event must originate from a booking. Drop pre-existing 0 rows defensively.
------------------------------------------------------------
DELETE FROM events WHERE booking_id = 0;
ALTER TABLE events
    ADD CONSTRAINT events_booking_id_fkey
    FOREIGN KEY (booking_id) REFERENCES bookings(id) ON DELETE RESTRICT;

------------------------------------------------------------
-- 6. journals.metadata: nullable -> NOT NULL DEFAULT '{}'
------------------------------------------------------------
UPDATE journals SET metadata = '{}'::jsonb WHERE metadata IS NULL;
ALTER TABLE journals ALTER COLUMN metadata SET DEFAULT '{}'::jsonb;
ALTER TABLE journals ALTER COLUMN metadata SET NOT NULL;

------------------------------------------------------------
-- 7. deposits.metadata, withdrawals.metadata: nullable -> NOT NULL DEFAULT '{}'
--    Tables are no longer used by application code (v2 unified into bookings)
--    but the schema still exists; enforce No-NULL while we have the chance.
------------------------------------------------------------
UPDATE deposits SET metadata = '{}'::jsonb WHERE metadata IS NULL;
ALTER TABLE deposits ALTER COLUMN metadata SET DEFAULT '{}'::jsonb;
ALTER TABLE deposits ALTER COLUMN metadata SET NOT NULL;

UPDATE withdrawals SET metadata = '{}'::jsonb WHERE metadata IS NULL;
ALTER TABLE withdrawals ALTER COLUMN metadata SET DEFAULT '{}'::jsonb;
ALTER TABLE withdrawals ALTER COLUMN metadata SET NOT NULL;

------------------------------------------------------------
-- 8. Append-only enforcement on journal_entries and journals.
--    journal_entries: never UPDATE, never DELETE.
--    journals: allow INSERT and the single allowed UPDATE that backfills
--      event_id (kept compatible with the existing flow); block any other UPDATE
--      and any DELETE.
------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_block_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % on % is not allowed; use a reversal journal instead',
        TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'check_violation';
END;
$$;

DROP TRIGGER IF EXISTS journal_entries_no_update ON journal_entries;
CREATE TRIGGER journal_entries_no_update
    BEFORE UPDATE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS journal_entries_no_delete ON journal_entries;
CREATE TRIGGER journal_entries_no_delete
    BEFORE DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS journals_no_delete ON journals;
CREATE TRIGGER journals_no_delete
    BEFORE DELETE ON journals
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

-- Allow only the safe "set event_id once" backfill on journals; block any
-- UPDATE that touches other columns.
CREATE OR REPLACE FUNCTION ledger_journals_block_arbitrary_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id              IS DISTINCT FROM OLD.id              OR
       NEW.journal_type_id IS DISTINCT FROM OLD.journal_type_id OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.total_debit     IS DISTINCT FROM OLD.total_debit     OR
       NEW.total_credit    IS DISTINCT FROM OLD.total_credit    OR
       NEW.metadata        IS DISTINCT FROM OLD.metadata        OR
       NEW.actor_id        IS DISTINCT FROM OLD.actor_id        OR
       NEW.source          IS DISTINCT FROM OLD.source          OR
       NEW.reversal_of     IS DISTINCT FROM OLD.reversal_of     OR
       NEW.created_at      IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS journals_no_arbitrary_update ON journals;
CREATE TRIGGER journals_no_arbitrary_update
    BEFORE UPDATE ON journals
    FOR EACH ROW EXECUTE FUNCTION ledger_journals_block_arbitrary_update();

------------------------------------------------------------
-- 9. Per-currency balance check: drop the O(N^2) ROW constraint trigger.
--
--    The original CONSTRAINT TRIGGER ... INITIALLY DEFERRED FOR EACH ROW
--    fires once per inserted row at COMMIT, and each invocation scans every
--    entry of the affected journal — O(N^2) per journal of size N.
--
--    PostgreSQL does not allow CONSTRAINT TRIGGER ... FOR EACH STATEMENT
--    (constraint triggers must be FOR EACH ROW). The simplest correct fix
--    is therefore to:
--      (a) drop the per-row trigger, and
--      (b) move the check into the application layer (postgres.LedgerStore)
--          where we issue exactly one validating SELECT per posted journal
--          inside the same transaction, *before* COMMIT.
--
--    Application-side validation runs in the same tx that inserted the
--    entries, so a failure rolls back the journal and its entries atomically.
--    Defense in depth: core.JournalInput.Validate() already checks per-currency
--    balance from the input slice; the SQL guard catches any drift between
--    the input and what was actually persisted (e.g. partial inserts, future
--    code changes that bypass core validation).
------------------------------------------------------------
DROP TRIGGER IF EXISTS trg_check_journal_currency_balance ON journal_entries;
DROP FUNCTION IF EXISTS check_journal_currency_balance();

------------------------------------------------------------
-- 10. rollup_queue.failed_attempts: avoid an infinite retry loop when
--     processing a queue item permanently fails (e.g. malformed normal_side).
--     Service code increments on every failed processItem and skips items
--     past a threshold.
------------------------------------------------------------
ALTER TABLE rollup_queue
    ADD COLUMN IF NOT EXISTS failed_attempts INTEGER NOT NULL DEFAULT 0;
