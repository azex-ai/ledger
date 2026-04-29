-- balance_trends.sql
-- Gap-filled daily balance time series.
--
-- Strategy:
--   1. generate_series expands the full date range so every day is present.
--   2. LEFT JOIN balance_snapshots brings in known data points.
--   3. A "last non-null" forward-fill is done with a subquery that groups by
--      a running count of non-null snapshots (standard PostgreSQL idiom).
--   4. Daily inflow/outflow are aggregated from journal_entries.
--
-- The caller is responsible for overriding today's balance with the live
-- checkpoint+delta value (done in Go, not SQL, to avoid coupling).

-- name: GetBalanceTrendGapFill :many
-- Returns one row per calendar day in [from_date, until_date] (inclusive).
WITH
date_series AS (
    SELECT d::date AS day
    FROM generate_series(
        sqlc.arg(from_date)::date,
        sqlc.arg(until_date)::date,
        interval '1 day'
    ) AS d
),
snapshots AS (
    SELECT
        snapshot_date,
        balance
    FROM balance_snapshots
    WHERE account_holder    = sqlc.arg(holder)::bigint
      AND currency_id       = sqlc.arg(currency_id)::bigint
      AND (sqlc.arg(classification_id)::bigint = 0 OR classification_id = sqlc.arg(classification_id)::bigint)
      AND snapshot_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(until_date)::date
),
daily_flows AS (
    SELECT
        (j.created_at AT TIME ZONE 'UTC')::date AS flow_date,
        COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0::numeric END), 0) AS inflow,
        COALESCE(SUM(CASE WHEN je.entry_type = 'debit'  THEN je.amount ELSE 0::numeric END), 0) AS outflow
    FROM journal_entries je
    JOIN journals j ON j.id = je.journal_id
    WHERE je.account_holder = sqlc.arg(holder)::bigint
      AND je.currency_id    = sqlc.arg(currency_id)::bigint
      AND (sqlc.arg(classification_id)::bigint = 0 OR je.classification_id = sqlc.arg(classification_id)::bigint)
      AND (j.created_at AT TIME ZONE 'UTC')::date
            BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(until_date)::date
    GROUP BY flow_date
),
joined AS (
    SELECT
        ds.day,
        s.balance                          AS snap_balance,
        COALESCE(df.inflow,  0::numeric)   AS inflow,
        COALESCE(df.outflow, 0::numeric)   AS outflow,
        -- Group counter: increments each time a non-null snapshot appears.
        -- All consecutive NULLs following a known value get the same group number,
        -- allowing MAX() to carry the last known balance forward.
        COUNT(s.balance) OVER (
            ORDER BY ds.day
            ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
        ) AS grp
    FROM date_series ds
    LEFT JOIN snapshots       s  ON s.snapshot_date = ds.day
    LEFT JOIN daily_flows     df ON df.flow_date    = ds.day
)
SELECT
    day,
    COALESCE(
        MAX(snap_balance) OVER (PARTITION BY grp ORDER BY day ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW),
        0::numeric
    ) AS balance,
    inflow,
    outflow
FROM joined
ORDER BY day;
