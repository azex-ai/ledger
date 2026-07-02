-- ListReservationsByAccount (postgres/sql/queries/reservations.sql) filters by
-- account_holder and an optional status, ordering by created_at DESC. The two
-- existing indexes on reservations are both partial (WHERE status = 'active')
-- and don't cover a query that can return any status, so this falls back to a
-- sequential scan + sort. A covering, non-partial index fixes that.
CREATE INDEX IF NOT EXISTS idx_reservations_account_created
    ON reservations (account_holder, currency_id, created_at DESC);
