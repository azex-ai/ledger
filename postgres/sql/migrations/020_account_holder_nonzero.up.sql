-- Reject account_holder = 0 at the database boundary. 0 is the "absent" sentinel
-- in this codebase (see CLAUDE.md "No NULL"); it must never appear on a real
-- ledger row. Two-phase ADD CONSTRAINT NOT VALID + VALIDATE so the enforcement
-- starts immediately for new writes while existing data is checked separately —
-- avoids a long blocking ACCESS EXCLUSIVE scan on already-deployed instances.
-- VALIDATE will fail loudly if any historical row violates the invariant; the
-- runbook documents the manual remediation step.

ALTER TABLE journal_entries
    ADD CONSTRAINT chk_journal_entries_account_holder_nonzero CHECK (account_holder <> 0) NOT VALID;
ALTER TABLE journal_entries VALIDATE CONSTRAINT chk_journal_entries_account_holder_nonzero;

ALTER TABLE reservations
    ADD CONSTRAINT chk_reservations_account_holder_nonzero CHECK (account_holder <> 0) NOT VALID;
ALTER TABLE reservations VALIDATE CONSTRAINT chk_reservations_account_holder_nonzero;

ALTER TABLE deposits
    ADD CONSTRAINT chk_deposits_account_holder_nonzero CHECK (account_holder <> 0) NOT VALID;
ALTER TABLE deposits VALIDATE CONSTRAINT chk_deposits_account_holder_nonzero;

ALTER TABLE withdrawals
    ADD CONSTRAINT chk_withdrawals_account_holder_nonzero CHECK (account_holder <> 0) NOT VALID;
ALTER TABLE withdrawals VALIDATE CONSTRAINT chk_withdrawals_account_holder_nonzero;

ALTER TABLE bookings
    ADD CONSTRAINT chk_bookings_account_holder_nonzero CHECK (account_holder <> 0) NOT VALID;
ALTER TABLE bookings VALIDATE CONSTRAINT chk_bookings_account_holder_nonzero;
