-- Close the last account_holder <> 0 gaps: every business table carries this
-- CHECK (004/006/007/008/012 inline, 020 re-asserted), but the two rollup
-- bookkeeping tables never got it. A holder=0 row here would silently
-- pollute aggregates (AggregateCheckpointsByClassification,
-- GetSystemSideCustodialBalance) that split on the holder sign.
ALTER TABLE balance_checkpoints DROP CONSTRAINT IF EXISTS chk_checkpoints_holder_nonzero;
ALTER TABLE balance_checkpoints ADD CONSTRAINT chk_checkpoints_holder_nonzero
    CHECK (account_holder <> 0);

ALTER TABLE rollup_queue DROP CONSTRAINT IF EXISTS chk_rollup_queue_holder_nonzero;
ALTER TABLE rollup_queue ADD CONSTRAINT chk_rollup_queue_holder_nonzero
    CHECK (account_holder <> 0);
