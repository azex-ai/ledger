CREATE TABLE reservations (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL,
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    reserved_amount NUMERIC(30,18) NOT NULL CHECK (reserved_amount > 0),
    settled_amount  NUMERIC(30,18),
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'settling', 'settled', 'released')),
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '15 minutes',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_settled_non_negative CHECK (settled_amount >= 0),
    CONSTRAINT chk_settled_lte_reserved CHECK (settled_amount <= reserved_amount)
);

CREATE INDEX idx_reservations_account_status ON reservations (account_holder, currency_id, status) WHERE status = 'active';
CREATE INDEX idx_reservations_expired ON reservations (expires_at) WHERE status = 'active';
