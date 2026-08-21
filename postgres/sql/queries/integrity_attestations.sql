-- P6 (batch attestation chain) reads/writes. See
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §3 -- this file is
-- exclusively owned by P6.

-- name: GetLatestLedgerAttestation :one
-- Returns the highest-seq attestation, or pgx.ErrNoRows if the chain has
-- never been started (the caller treats that as seq=0 / GenesisRoot).
SELECT * FROM ledger_attestations ORDER BY seq DESC LIMIT 1;

-- name: GetLedgerAttestationBySeq :one
SELECT * FROM ledger_attestations WHERE seq = $1;

-- name: ListLedgerAttestationsFrom :many
-- Paginated chain walk for ledger-cli verify's seq-continuity /
-- prev_root-linkage check -- ordered ascending so each row's prev_root can
-- be compared against the previous row's root_hash in one linear pass.
SELECT * FROM ledger_attestations
WHERE seq >= sqlc.arg(from_seq)::bigint
ORDER BY seq ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: InsertLedgerAttestation :one
INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, merkle_root, prev_root, root_hash, signature, key_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListUncoveredEntries :many
-- Entries with no entry_attestations row yet, oldest id first. Ordinary
-- LEFT JOIN anti-join, deliberately NOT bounded by an id/time window --
-- see core/attestation.go's package doc comment and design doc §8.2: a
-- late-arriving entry from a different (holder, currency) pair, committed
-- after a batch that closed out on a higher id, must still surface here on
-- the next poll (coverage is a queryable fact via entry_attestations, not
-- an id-range assumption).
SELECT je.id, je.journal_id, je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount, je.effective_at
FROM journal_entries je
LEFT JOIN entry_attestations ea ON ea.entry_id = je.id
WHERE ea.entry_id IS NULL
ORDER BY je.id ASC
LIMIT sqlc.arg(batch_size)::int;

-- name: InsertEntryAttestations :exec
-- Bulk-covers every id in entry_ids under the same seq, in one round trip.
INSERT INTO entry_attestations (entry_id, seq)
SELECT e, sqlc.arg(seq)::bigint FROM unnest(sqlc.arg(entry_ids)::bigint[]) AS e;

-- name: ListEntriesForAttestation :many
-- Re-fetches exactly the entries a given seq covered, in the same id order
-- ListUncoveredEntries would have produced them in -- used by ledger-cli
-- verify to recompute core.CanonicalBatchDigest from live DB content and
-- compare it against the stored batch_digest.
SELECT je.id, je.journal_id, je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount, je.effective_at
FROM entry_attestations ea
JOIN journal_entries je ON je.id = ea.entry_id
WHERE ea.seq = $1
ORDER BY je.id ASC;
