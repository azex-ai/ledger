-- Reverse of 018. Dropping the NaN constraints cannot fail on data (removing
-- a constraint never violates one); dropping anchor_observations discards the
-- anchor-regression memory, which is the point of a rollback.
--
-- The table is owned by ledger_owner (see the up migration), so the same
-- temporary membership upgrade is needed to drop it.

DROP TABLE IF EXISTS anchor_observations;

ALTER TABLE booking_transition_receipts    DROP CONSTRAINT IF EXISTS chk_booking_transition_receipts_amount_not_nan;
ALTER TABLE reservation_operation_receipts DROP CONSTRAINT IF EXISTS chk_reservation_operation_receipts_amount_not_nan;
ALTER TABLE checkpoint_rebuilds            DROP CONSTRAINT IF EXISTS chk_checkpoint_rebuilds_drift_not_nan;
ALTER TABLE checkpoint_rebuilds            DROP CONSTRAINT IF EXISTS chk_checkpoint_rebuilds_new_balance_not_nan;
ALTER TABLE checkpoint_rebuilds            DROP CONSTRAINT IF EXISTS chk_checkpoint_rebuilds_previous_balance_not_nan;
ALTER TABLE account_policies               DROP CONSTRAINT IF EXISTS chk_account_policies_min_balance_not_nan;
ALTER TABLE withdrawals                    DROP CONSTRAINT IF EXISTS chk_withdrawals_amount_not_nan;
ALTER TABLE deposits                       DROP CONSTRAINT IF EXISTS chk_deposits_actual_amount_not_nan;
ALTER TABLE deposits                       DROP CONSTRAINT IF EXISTS chk_deposits_expected_amount_not_nan;
ALTER TABLE events                         DROP CONSTRAINT IF EXISTS chk_events_settled_amount_not_nan;
ALTER TABLE events                         DROP CONSTRAINT IF EXISTS chk_events_amount_not_nan;
ALTER TABLE bookings                       DROP CONSTRAINT IF EXISTS chk_bookings_settled_amount_not_nan;
ALTER TABLE bookings                       DROP CONSTRAINT IF EXISTS chk_bookings_amount_not_nan;
ALTER TABLE reservation_settlement_legs    DROP CONSTRAINT IF EXISTS chk_reservation_settlement_legs_amount_not_nan;
ALTER TABLE reservations                   DROP CONSTRAINT IF EXISTS chk_reservations_settled_amount_not_nan;
ALTER TABLE reservations                   DROP CONSTRAINT IF EXISTS chk_reservations_reserved_amount_not_nan;
ALTER TABLE system_rollups                 DROP CONSTRAINT IF EXISTS chk_system_rollups_total_balance_not_nan;
ALTER TABLE balance_snapshots              DROP CONSTRAINT IF EXISTS chk_balance_snapshots_balance_not_nan;
ALTER TABLE balance_checkpoints            DROP CONSTRAINT IF EXISTS chk_balance_checkpoints_balance_not_nan;
ALTER TABLE journal_entries                DROP CONSTRAINT IF EXISTS chk_journal_entries_amount_not_nan;
ALTER TABLE journals                       DROP CONSTRAINT IF EXISTS chk_journals_total_credit_not_nan;
ALTER TABLE journals                       DROP CONSTRAINT IF EXISTS chk_journals_total_debit_not_nan;
