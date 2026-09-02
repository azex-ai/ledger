-- name: GetChainCursor :one
SELECT * FROM chain_cursors WHERE chain_id = $1;

-- name: SetChainCursor :exec
-- Upsert: first call for a chain initializes the row, subsequent calls
-- advance it. Monotonicity (last_scanned_block only ever moves forward) is
-- enforced HERE, by the WHERE clause below -- the same shape
-- UpsertBalanceCheckpoint (checkpoints.sql) already uses.
--
-- It used to say monotonicity was "an orchestration-layer invariant
-- (service/)", and service/ did not implement it (concurrency.md B-m7): with
-- two replicas both running the watcher, a slow one that read cursor=100,
-- stalled, then wrote 200 while a fast one had already reached 300 would drag
-- the cursor backwards. Re-scanning is idempotent so no deposit was lost, but
-- an invariant nobody implements is worse than no invariant -- and the
-- fail-closed cursor semantics I-52 now depends on (a window is only marked
-- scanned once every sighting in it is ingested or dead-lettered) would be
-- silently undone by a backwards write.
INSERT INTO chain_cursors (chain_id, last_scanned_block)
VALUES ($1, $2)
ON CONFLICT (chain_id) DO UPDATE SET
    last_scanned_block = EXCLUDED.last_scanned_block,
    updated_at          = now()
WHERE chain_cursors.last_scanned_block < EXCLUDED.last_scanned_block;
