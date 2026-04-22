CREATE TABLE webhook_subscribers (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL,
    secret           TEXT NOT NULL DEFAULT '',
    filter_class     TEXT NOT NULL DEFAULT '',
    filter_to_status TEXT NOT NULL DEFAULT '',
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
