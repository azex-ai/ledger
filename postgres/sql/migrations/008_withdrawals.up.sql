CREATE TABLE withdrawals (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    amount          NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    status          TEXT NOT NULL DEFAULT 'locked'
                    CHECK (status IN ('locked', 'reserved', 'reviewing', 'processing', 'confirmed', 'failed', 'expired')),
    channel_name    TEXT NOT NULL,
    channel_ref     TEXT UNIQUE,
    reservation_id  BIGINT REFERENCES reservations(id),
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    metadata        JSONB DEFAULT '{}',
    review_required BOOLEAN NOT NULL DEFAULT false,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_withdrawals_account ON withdrawals (account_holder, currency_id, status);
