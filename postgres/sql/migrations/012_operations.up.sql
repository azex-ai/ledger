CREATE TABLE bookings (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_id BIGINT NOT NULL,
    account_holder    BIGINT NOT NULL CHECK (account_holder <> 0),
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

    CONSTRAINT uq_bookings_idempotency UNIQUE (idempotency_key),
    CONSTRAINT chk_bookings_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_bookings_settled_non_negative CHECK (settled_amount >= 0)
);

CREATE UNIQUE INDEX uq_bookings_channel_ref
    ON bookings (channel_name, channel_ref)
    WHERE channel_ref != '';

CREATE INDEX idx_bookings_holder_class
    ON bookings (account_holder, classification_id, status);

CREATE INDEX idx_bookings_expires
    ON bookings (expires_at)
    WHERE expires_at != 'epoch';
