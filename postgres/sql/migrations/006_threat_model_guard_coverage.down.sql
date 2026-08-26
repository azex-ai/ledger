DROP TRIGGER IF EXISTS entry_templates_audit ON entry_templates;
DROP TRIGGER IF EXISTS journal_types_audit ON journal_types;
DROP TRIGGER IF EXISTS classifications_audit ON classifications;
DROP TRIGGER IF EXISTS currencies_audit ON currencies;
DROP FUNCTION IF EXISTS ledger_log_config_table_change();
DROP TABLE IF EXISTS config_table_changes;

DROP TRIGGER IF EXISTS reconcile_scan_cursors_audit ON reconcile_scan_cursors;
DROP FUNCTION IF EXISTS ledger_log_reconcile_scan_cursor_change();
DROP TABLE IF EXISTS reconcile_scan_cursor_changes;

GRANT UPDATE ON public.booking_transition_receipts    TO ledger_app;
GRANT UPDATE ON public.reservation_operation_receipts  TO ledger_app;
GRANT UPDATE ON public.reservation_settlement_legs     TO ledger_app;
GRANT UPDATE ON public.account_policy_changes          TO ledger_app;

DROP TRIGGER IF EXISTS booking_transition_receipts_no_delete ON booking_transition_receipts;
DROP TRIGGER IF EXISTS booking_transition_receipts_no_update ON booking_transition_receipts;
DROP TRIGGER IF EXISTS reservation_operation_receipts_no_delete ON reservation_operation_receipts;
DROP TRIGGER IF EXISTS reservation_operation_receipts_no_update ON reservation_operation_receipts;
DROP TRIGGER IF EXISTS reservation_settlement_legs_no_delete ON reservation_settlement_legs;
DROP TRIGGER IF EXISTS reservation_settlement_legs_no_update ON reservation_settlement_legs;
DROP TRIGGER IF EXISTS account_policy_changes_no_delete ON account_policy_changes;
DROP TRIGGER IF EXISTS account_policy_changes_no_update ON account_policy_changes;

DROP TRIGGER IF EXISTS events_mutation_guard ON events;
DROP FUNCTION IF EXISTS ledger_events_guard();

DROP TRIGGER IF EXISTS bookings_mutation_guard ON bookings;
DROP FUNCTION IF EXISTS ledger_bookings_guard();

DROP TRIGGER IF EXISTS account_policies_mutation_guard ON account_policies;
DROP FUNCTION IF EXISTS ledger_account_policies_guard();
