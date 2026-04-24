CREATE TABLE balance_checkpoints (
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    balance           NUMERIC(30,18) NOT NULL DEFAULT 0,
    last_entry_id     BIGINT NOT NULL DEFAULT 0,
    last_entry_at     TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_holder, currency_id, classification_id)
);

CREATE TABLE rollup_queue (
    id                BIGSERIAL PRIMARY KEY,
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    claimed_until     TIMESTAMPTZ,
    processed_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_rollup_queue_pending_dimension
    ON rollup_queue (account_holder, currency_id, classification_id)
    WHERE processed_at IS NULL;

CREATE INDEX idx_rollup_queue_pending ON rollup_queue (created_at, id) WHERE processed_at IS NULL;
