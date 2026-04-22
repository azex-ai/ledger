CREATE TABLE operations (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_id BIGINT NOT NULL,
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    amount            NUMERIC(30,18) NOT NULL,
    settled_amount    NUMERIC(30,18) NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    channel_name      TEXT NOT NULL DEFAULT '',
    channel_ref       TEXT NOT NULL DEFAULT '',
    reservation_id    BIGINT NOT NULL DEFAULT 0,
    journal_id        BIGINT NOT NULL DEFAULT 0,
    idempotency_key   TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}',
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_operations_idempotency UNIQUE (idempotency_key)
);

CREATE UNIQUE INDEX uq_operations_channel_ref
    ON operations (channel_name, channel_ref)
    WHERE channel_ref != '';

CREATE INDEX idx_operations_holder_class
    ON operations (account_holder, classification_id, status);

CREATE INDEX idx_operations_expires
    ON operations (expires_at)
    WHERE expires_at != 'epoch';
