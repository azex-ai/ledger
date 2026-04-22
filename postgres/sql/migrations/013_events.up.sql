CREATE TABLE events (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_code TEXT NOT NULL,
    operation_id        BIGINT NOT NULL DEFAULT 0,
    account_holder      BIGINT NOT NULL DEFAULT 0,
    currency_id         BIGINT NOT NULL DEFAULT 0,
    from_status         TEXT NOT NULL DEFAULT '',
    to_status           TEXT NOT NULL,
    amount              NUMERIC(30,18) NOT NULL DEFAULT 0,
    settled_amount      NUMERIC(30,18) NOT NULL DEFAULT 0,
    journal_id          BIGINT NOT NULL DEFAULT 0,
    metadata            JSONB NOT NULL DEFAULT '{}',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    delivery_status     TEXT NOT NULL DEFAULT 'pending',
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 10,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_delivery_pending
    ON events (next_attempt_at)
    WHERE delivery_status = 'pending';

CREATE INDEX idx_events_operation
    ON events (operation_id)
    WHERE operation_id != 0;
