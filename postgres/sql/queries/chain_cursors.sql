-- name: GetChainCursor :one
SELECT * FROM chain_cursors WHERE chain_id = $1;

-- name: SetChainCursor :exec
-- Upsert: first call for a chain initializes the row, subsequent calls
-- advance it. The watcher is expected to call this monotonically
-- (last_scanned_block only moves forward); this query does not enforce
-- monotonicity -- that is an orchestration-layer invariant (service/), not a
-- storage one.
INSERT INTO chain_cursors (chain_id, last_scanned_block)
VALUES ($1, $2)
ON CONFLICT (chain_id) DO UPDATE SET
    last_scanned_block = EXCLUDED.last_scanned_block,
    updated_at          = now();
