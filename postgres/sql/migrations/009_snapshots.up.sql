CREATE TABLE balance_snapshots (
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    snapshot_date     DATE NOT NULL,
    balance           NUMERIC(30,18) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_holder, currency_id, classification_id, snapshot_date)
);

CREATE INDEX idx_snapshots_date ON balance_snapshots (snapshot_date);

CREATE TABLE system_rollups (
    currency_id       BIGINT NOT NULL REFERENCES currencies(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    total_balance     NUMERIC(30,18) NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (currency_id, classification_id)
);
