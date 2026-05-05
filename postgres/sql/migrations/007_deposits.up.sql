CREATE TABLE deposits (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    expected_amount NUMERIC(30,18) NOT NULL CHECK (expected_amount > 0),
    actual_amount   NUMERIC(30,18),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'confirming', 'confirmed', 'failed', 'expired')),
    channel_name    TEXT NOT NULL,
    channel_ref     TEXT UNIQUE,
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    metadata        JSONB DEFAULT '{}',
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_deposits_account ON deposits (account_holder, currency_id, status);
