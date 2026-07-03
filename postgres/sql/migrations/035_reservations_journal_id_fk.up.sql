-- Restore referential integrity on reservations.journal_id — the one FK that
-- migration 018 forgot. 017 converted the column to NOT NULL DEFAULT 0 and
-- dropped its FK; 018 restored every sibling (bookings.journal_id,
-- bookings.reservation_id, events.journal_id) as a nullable FK-target
-- exception (I-7) but skipped reservations. Until now a wrong journal_id was
-- silently accepted, with only the after-the-fact ReconcileOrphanReservations
-- check as a net.
--
-- Same shape as the 018 exceptions: NULL = "no journal linked", non-NULL
-- must reference a real journal.
ALTER TABLE reservations ALTER COLUMN journal_id DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id DROP DEFAULT;
UPDATE reservations SET journal_id = NULL WHERE journal_id = 0;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_journal_id_fkey;
ALTER TABLE reservations ADD CONSTRAINT reservations_journal_id_fkey
    FOREIGN KEY (journal_id) REFERENCES journals(id);
