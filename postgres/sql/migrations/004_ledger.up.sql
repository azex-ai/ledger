CREATE TABLE journals (
    id              BIGSERIAL PRIMARY KEY,
    journal_type_id BIGINT NOT NULL REFERENCES journal_types(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    total_debit     NUMERIC(30,18) NOT NULL,
    total_credit    NUMERIC(30,18) NOT NULL,
    metadata        JSONB DEFAULT '{}',
    actor_id        BIGINT,
    source          TEXT,
    reversal_of     BIGINT REFERENCES journals(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_journal_balance CHECK (total_debit = total_credit),
    CONSTRAINT chk_journal_nonzero CHECK (total_debit > 0)
);

CREATE INDEX idx_journals_created ON journals (created_at);

CREATE TABLE journal_entries (
    id                BIGSERIAL,
    journal_id        BIGINT NOT NULL REFERENCES journals(id),
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL REFERENCES currencies(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    entry_type        TEXT NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    amount            NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY RANGE (created_at);

CREATE INDEX idx_entries_account_id ON journal_entries (account_holder, currency_id, classification_id, id);
CREATE INDEX idx_entries_journal ON journal_entries (journal_id);
