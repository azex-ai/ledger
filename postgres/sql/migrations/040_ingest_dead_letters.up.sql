-- Ingest dead letters (docs/plans/2026-07-11-crypto-deposit-sweep-design.md
-- §6): IngestDeposit hitting core.ErrConflict on CreateBooking means the
-- watcher and webhook ingestion paths derived different payloads for what
-- should be the identical sighting -- a normalization bug, not a transient
-- error. These rows are never auto-retried; on-call reviews and reconciles
-- manually. uid is NOT NULL from the start -- this table does not exist yet,
-- no backfill dance needed (contrast migration 031).
CREATE TABLE IF NOT EXISTS ingest_dead_letters (
    id              BIGSERIAL PRIMARY KEY,
    uid             UUID NOT NULL,
    chain_id        BIGINT NOT NULL,
    tx_hash         TEXT NOT NULL,
    txlog_seq       INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL,
    reason          TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_ingest_dead_letters_uid ON ingest_dead_letters (uid);
-- One dead-letter row per conflicting idempotency key: repeated conflicts on
-- the same key (e.g. the watcher retrying a sighting it can never reconcile)
-- must not spam the table -- INSERT ... ON CONFLICT DO NOTHING relies on this.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ingest_dead_letters_idempotency_key ON ingest_dead_letters (idempotency_key);
CREATE INDEX IF NOT EXISTS idx_ingest_dead_letters_chain_tx ON ingest_dead_letters (chain_id, tx_hash);
