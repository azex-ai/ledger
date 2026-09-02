-- Reversible: dropping the constraints restores the pre-022 state exactly.
-- The orphan rows the up-migration deleted are not restored (they were
-- derived cache/queue rows pointing at dimensions that do not exist; there
-- is nothing to restore them from, and their absence costs only a
-- recomputation).
ALTER TABLE balance_checkpoints
    DROP CONSTRAINT IF EXISTS fk_checkpoints_currency,
    DROP CONSTRAINT IF EXISTS fk_checkpoints_classification;

ALTER TABLE rollup_queue
    DROP CONSTRAINT IF EXISTS fk_rollup_queue_currency,
    DROP CONSTRAINT IF EXISTS fk_rollup_queue_classification;

ALTER TABLE balance_snapshots
    DROP CONSTRAINT IF EXISTS fk_snapshots_currency,
    DROP CONSTRAINT IF EXISTS fk_snapshots_classification;
