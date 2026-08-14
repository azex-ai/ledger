CREATE TABLE registration_rescans (
    id            BIGSERIAL PRIMARY KEY,
    uid           UUID NOT NULL UNIQUE,
    chain_id      BIGINT NOT NULL CHECK (chain_id > 0),
    address       TEXT NOT NULL,
    next_block    BIGINT NOT NULL CHECK (next_block >= 0),
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'running', 'completed')),
    attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until TIMESTAMPTZ,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chain_id, address)
);

CREATE INDEX idx_registration_rescans_claim
    ON registration_rescans (available_at, claimed_until)
    WHERE status <> 'completed';
