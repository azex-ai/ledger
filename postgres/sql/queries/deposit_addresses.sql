-- name: UpsertDepositAddress :one
-- Idempotent registration: account_holder is UNIQUE, so a second call for a
-- holder that already has a row is a no-op that returns the existing row
-- unchanged (the "DO UPDATE SET col = col" trick, since plain DO NOTHING
-- would make RETURNING yield no row on conflict). Callers must not pass a
-- different address/factory/init_hash for an already-registered holder --
-- this query does not detect or reject that mismatch; it silently keeps the
-- original row.
INSERT INTO deposit_addresses (uid, account_holder, address, factory, init_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_holder) DO UPDATE SET account_holder = EXCLUDED.account_holder
RETURNING *;

-- name: GetDepositAddressByHolder :one
SELECT * FROM deposit_addresses WHERE account_holder = $1;

-- name: GetDepositAddressByAddress :one
-- Reverse lookup used by the watcher/ingestion path (observed `to` address ->
-- holder). address must be passed in the same canonical EIP-55 casing the
-- row was written with (see migration 039's index comment).
SELECT * FROM deposit_addresses WHERE address = $1;

-- name: ListDepositAddresses :many
-- Feeds the watcher's `to ∈ registry` filter set. Unbounded on purpose (this
-- period): the registry is expected to stay small enough to hold in memory;
-- revisit with pagination if that assumption breaks.
SELECT * FROM deposit_addresses ORDER BY id;
