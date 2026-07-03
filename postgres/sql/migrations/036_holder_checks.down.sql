ALTER TABLE balance_checkpoints DROP CONSTRAINT IF EXISTS chk_checkpoints_holder_nonzero;
ALTER TABLE rollup_queue DROP CONSTRAINT IF EXISTS chk_rollup_queue_holder_nonzero;
