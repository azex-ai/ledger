ALTER TABLE journal_entries DROP CONSTRAINT IF EXISTS chk_journal_entries_account_holder_nonzero;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS chk_reservations_account_holder_nonzero;
ALTER TABLE deposits DROP CONSTRAINT IF EXISTS chk_deposits_account_holder_nonzero;
ALTER TABLE withdrawals DROP CONSTRAINT IF EXISTS chk_withdrawals_account_holder_nonzero;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS chk_bookings_account_holder_nonzero;
