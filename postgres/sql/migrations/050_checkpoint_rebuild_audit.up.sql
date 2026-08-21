-- 050_checkpoint_rebuild_audit.up.sql
--
-- P2 follow-up (integrity-hardening wave). Team-lead review of #6 flagged a
-- gap: RebuildCheckpoint repairing a poisoned checkpoint has the exact same
-- evidence-destroying property that automatic repair does (design doc §4:
-- "攻击仍在进行时自动覆盖会毁掉取证证据") -- a human deciding to run it
-- doesn't change that the drift disappears the moment it's overwritten,
-- leaving only a log line that can rotate or be lost. This table is the
-- durable, append-only record of every rebuild: the drift is the evidence,
-- and it must outlive the log.
--
-- Migration number 050 (not 044): 043 is this same P2 task; 044-048 are
-- P3-P7's assigned numbers (docs/plans/2026-08-21-integrity-hardening-contracts.md
-- §1); 049 is reserved for P1's migrate-phase DATABASE_URL cutover. Team lead
-- assigned 050 for this follow-up during review of #6.

CREATE TABLE checkpoint_rebuilds (
    id                      BIGSERIAL PRIMARY KEY,
    uid                     UUID NOT NULL,
    account_holder          BIGINT NOT NULL,
    currency_id             BIGINT NOT NULL REFERENCES currencies(id),
    classification_id       BIGINT NOT NULL REFERENCES classifications(id),
    previous_balance        NUMERIC(30,18) NOT NULL,
    previous_last_entry_id  BIGINT NOT NULL,
    new_balance             NUMERIC(30,18) NOT NULL,
    new_last_entry_id       BIGINT NOT NULL,
    -- drift = previous_balance - new_balance (same sign convention as
    -- ReconcileAccount's Detail.Drift: actual-the-checkpoint-claimed minus
    -- expected-from-entries). A non-zero row is exactly the evidence a
    -- poisoned checkpoint existed; a zero-drift row means RebuildCheckpoint
    -- ran as a no-op confirmation, not a repair.
    drift                   NUMERIC(30,18) NOT NULL,
    actor_id                BIGINT NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_checkpoint_rebuilds_uid ON checkpoint_rebuilds (uid);
CREATE INDEX idx_checkpoint_rebuilds_dimension
    ON checkpoint_rebuilds (account_holder, currency_id, classification_id);
-- The forensically interesting rows: fast "show me every real repair" scan.
CREATE INDEX idx_checkpoint_rebuilds_nonzero_drift
    ON checkpoint_rebuilds (created_at) WHERE drift <> 0;

-- Append-only: reuses the same block-mutation guard 018 defined for
-- journals/journal_entries. A rebuild record must never be edited or
-- deleted -- doing so would defeat the exact property this table exists for.
CREATE TRIGGER checkpoint_rebuilds_no_update
    BEFORE UPDATE ON checkpoint_rebuilds
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER checkpoint_rebuilds_no_delete
    BEFORE DELETE ON checkpoint_rebuilds
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();
