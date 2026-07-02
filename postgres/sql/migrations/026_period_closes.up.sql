-- Append-only accounting period close log. See
-- docs/plans/2026-07-02-financial-core-hardening-design.md §2.
--
-- The active close line at any moment is the row with the latest created_at
-- (latest-row-wins). Reopening a period = appending a row with an earlier
-- close_before; nothing is ever updated or deleted, so the full history stays
-- auditable.
CREATE TABLE period_closes (
    id           BIGSERIAL PRIMARY KEY,
    close_before TIMESTAMPTZ NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    actor_id     BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_period_closes_created ON period_closes (created_at DESC);
