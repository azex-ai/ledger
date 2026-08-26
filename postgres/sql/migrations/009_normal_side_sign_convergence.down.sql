REVOKE ALL ON FUNCTION ledger_signed_delta(text, numeric, numeric) FROM ledger_app, ledger_ro;
DROP FUNCTION IF EXISTS ledger_signed_delta(text, numeric, numeric);

REVOKE ALL ON FUNCTION ledger_signed_amount(text, text, numeric) FROM ledger_app, ledger_ro;
DROP FUNCTION IF EXISTS ledger_signed_amount(text, text, numeric);

REVOKE ALL ON FUNCTION ledger_reject_unknown_normal_side(text) FROM ledger_app, ledger_ro;
DROP FUNCTION IF EXISTS ledger_reject_unknown_normal_side(text);
