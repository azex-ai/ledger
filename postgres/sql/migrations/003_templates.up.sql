CREATE TABLE entry_templates (
    id              BIGSERIAL PRIMARY KEY,
    code            TEXT UNIQUE NOT NULL,
    name            TEXT NOT NULL,
    journal_type_id BIGINT NOT NULL REFERENCES journal_types(id),
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE entry_template_lines (
    id                BIGSERIAL PRIMARY KEY,
    template_id       BIGINT NOT NULL REFERENCES entry_templates(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    entry_type        TEXT NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    holder_role       TEXT NOT NULL CHECK (holder_role IN ('user', 'system')),
    amount_key        TEXT NOT NULL,
    sort_order        INT NOT NULL DEFAULT 0
);
