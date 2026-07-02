-- ListReservationsByAccount (postgres/sql/queries/reservations.sql) filters by
-- account_holder and an optional status, ordering by created_at DESC. The two
-- existing indexes on reservations are both partial (WHERE status = 'active')
-- and don't cover a query that can return any status, so this falls back to a
-- sequential scan + sort. A covering, non-partial index fixes that.
-- currency_id is deliberately excluded: the query does not filter on it, and
-- placing it between account_holder and created_at would prevent the index
-- from satisfying the ORDER BY.
CREATE INDEX IF NOT EXISTS idx_reservations_account_created
    ON reservations (account_holder, created_at DESC);
