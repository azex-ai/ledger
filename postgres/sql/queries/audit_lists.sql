-- audit_lists.sql
-- Read-only queries for the audit layer.
-- All list queries use keyset (cursor) pagination on journal.id ASC.

-- name: ListJournalsByAccount :many
-- Returns journals whose entries touch (holder, currency_id[, classification_id]).
-- classification_id = 0 means "all classifications".
-- Keyset cursor: id > cursor_id, ordered by id ASC.
-- since/until zero value (year 0001) is treated as unbounded.
SELECT DISTINCT j.*
FROM journals j
JOIN journal_entries je ON je.journal_id = j.id
WHERE je.account_holder = sqlc.arg(holder)::bigint
  AND je.currency_id    = sqlc.arg(currency_id)::bigint
  AND (sqlc.arg(classification_id)::bigint = 0 OR je.classification_id = sqlc.arg(classification_id)::bigint)
  AND (sqlc.arg(since)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR j.created_at >= sqlc.arg(since)::timestamptz)
  AND (sqlc.arg(until)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR j.created_at <= sqlc.arg(until)::timestamptz)
  AND j.id > sqlc.arg(cursor_id)::bigint
ORDER BY j.id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListJournalsByTimeRange :many
-- Returns journals created within [since, until].
-- since/until zero value (year 0001) is treated as unbounded on that side.
SELECT *
FROM journals
WHERE (sqlc.arg(since)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR created_at >= sqlc.arg(since)::timestamptz)
  AND (sqlc.arg(until)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR created_at <= sqlc.arg(until)::timestamptz)
  AND id > sqlc.arg(cursor_id)::bigint
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: TraceBookingEvents :many
-- Returns all events for a booking in ascending order.
SELECT *
FROM events
WHERE booking_id = $1
ORDER BY id ASC;

-- name: TraceBookingJournals :many
-- Returns all journals linked to events for a booking, ordered by id.
SELECT DISTINCT j.*
FROM journals j
JOIN events e ON e.journal_id = j.id
WHERE e.booking_id = $1
ORDER BY j.id ASC;

-- name: GetReversalChain :many
-- Returns the reversal chain for a journal.
-- Includes: the root journal + any journals that reference it (direct reversals).
-- Transitively follows reversal_of links up the chain (up to the root)
-- and down the chain (journals that reverse the given journal or any of its ancestors).
WITH RECURSIVE chain AS (
    -- Anchor: walk UP to find the root of the chain
    SELECT j.id AS journal_id, j.reversal_of
    FROM journals j
    WHERE j.id = $1

    UNION ALL

    SELECT anc.id AS journal_id, anc.reversal_of
    FROM journals anc
    JOIN chain c ON anc.id = c.reversal_of
    WHERE anc.reversal_of IS NOT NULL
),
root AS (
    SELECT journal_id FROM chain WHERE reversal_of IS NULL
    LIMIT 1
),
full_chain AS (
    -- Walk DOWN from root to find all reversals
    SELECT j.id AS journal_id, j.reversal_of
    FROM journals j
    WHERE j.id = (SELECT journal_id FROM root)

    UNION ALL

    SELECT j.id AS journal_id, j.reversal_of
    FROM journals j
    JOIN full_chain fc ON j.reversal_of = fc.journal_id
)
SELECT DISTINCT j.*
FROM journals j
JOIN full_chain fc ON fc.journal_id = j.id
ORDER BY j.id ASC;
