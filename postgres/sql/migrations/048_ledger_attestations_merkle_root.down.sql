REVOKE SELECT ON ledger_attestations FROM ledger_ro;
REVOKE SELECT ON entry_attestations FROM ledger_ro;
REVOKE SELECT, INSERT ON ledger_attestations FROM ledger_app;
REVOKE SELECT, INSERT ON entry_attestations FROM ledger_app;

ALTER TABLE ledger_attestations
    DROP COLUMN IF EXISTS merkle_root;
