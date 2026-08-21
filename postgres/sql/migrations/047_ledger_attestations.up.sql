-- 047_ledger_attestations.up.sql
--
-- P6 of the integrity-hardening wave
-- (docs/plans/2026-08-21-tamper-evident-ledger-design.md §8,
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §1/§4/§5): batch
-- attestation chain over journal_entries, so a row DELETE or a
-- history-wide rewrite (both invisible to P5's per-journal signatures,
-- which only prove "this row was authorized when written", not "this row
-- still exists" or "the whole history hasn't been renumbered") becomes
-- detectable.
--
--   ledger_attestations -- one row per batch ("checkpoint" in the hash-chain
--     sense). seq is a gapless sequence: a missing seq is a truncated
--     batch, immediately visible without needing an external comparison.
--     prev_root chains each batch to the one before it (genesis = 32 zero
--     bytes), so rewriting ANY historical batch's content changes every
--     root_hash after it -- the external anchor (core.Anchor) only needs
--     to remember the LATEST root_hash to make that rewrite detectable.
--   entry_attestations -- a side table (not a column on journal_entries)
--     recording which batch (seq) covered each entry. A side table, not a
--     "covered" flag column, because:
--       1. journal_entries' no-UPDATE trigger is one of the few hard
--          guarantees left in this schema (018) -- opening it for a
--          "coverage" flag would be exactly the kind of guard-with-an-
--          exception that decayed in A4 (journals.event_id's set-once
--          promise that was never actually implemented).
--       2. coverage becomes a plain queryable fact (LEFT JOIN ... WHERE
--          seq IS NULL) instead of an id-range assumption -- entries can
--          commit out of id order across different (holder, currency)
--          pairs (I-5's ordering guarantee only holds WITHIN one pair), so
--          an id-range boundary would let a late-arriving small-id entry
--          slip through a gap no seq-continuity check could ever notice.
------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_attestations (
    id           BIGSERIAL PRIMARY KEY,
    uid          UUID   NOT NULL UNIQUE,
    seq          BIGINT NOT NULL UNIQUE,
    entry_count  BIGINT NOT NULL,
    batch_digest BYTEA  NOT NULL,
    prev_root    BYTEA  NOT NULL,
    root_hash    BYTEA  NOT NULL,
    signature    BYTEA  NOT NULL,
    key_id       TEXT   NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ledger_attestations_seq ON ledger_attestations (seq);

CREATE TABLE IF NOT EXISTS entry_attestations (
    entry_id BIGINT NOT NULL,
    seq      BIGINT NOT NULL REFERENCES ledger_attestations(seq),
    PRIMARY KEY (entry_id)
);

CREATE INDEX IF NOT EXISTS idx_entry_attestations_seq ON entry_attestations (seq);

------------------------------------------------------------
-- Append-only: reuses 018's ledger_block_mutation(), the same trigger
-- function journals/journal_entries use.
------------------------------------------------------------
DROP TRIGGER IF EXISTS ledger_attestations_no_update ON ledger_attestations;
CREATE TRIGGER ledger_attestations_no_update
    BEFORE UPDATE ON ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS ledger_attestations_no_delete ON ledger_attestations;
CREATE TRIGGER ledger_attestations_no_delete
    BEFORE DELETE ON ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS entry_attestations_no_update ON entry_attestations;
CREATE TRIGGER entry_attestations_no_update
    BEFORE UPDATE ON entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS entry_attestations_no_delete ON entry_attestations;
CREATE TRIGGER entry_attestations_no_delete
    BEFORE DELETE ON entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();
