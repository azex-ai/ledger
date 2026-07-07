-- Holder-scoped wallet read surface projections
-- (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.3).

-- name: ListHolderTransactionRows :many
-- Raw rows of the holder transaction view: one row per (journal, currency)
-- net aggregate for the holder, newest journal first.
--
-- Pagination is at JOURNAL granularity: page_journals picks the next
-- page_limit journal ids, so a multi-currency journal's rows are never split
-- across pages and the cursor (last journal id) stays correct.
--
-- Zero-net rows (holder-internal moves between own classifications) ARE
-- returned here and filtered by the store layer: they must still advance the
-- cursor, otherwise a page of all-internal journals would read as end-of-list.
--
-- net_amount sign convention: an entry increases the holder's balance when
-- its entry_type equals the classification's normal_side. Positive net = "in".
--
-- Only role-bearing classifications (available/pending/locked) participate —
-- the same "spendable money" scope as BalanceBreakdown. Role-less holder-side
-- trackers (fee_expense, ...) are bookkeeping detail: including them would
-- net a fee charge to zero and hide it from the user.
--
-- kind_label fallback chain (§3.5): single classification with a non-empty
-- display_label -> that label; else journal type display_label; else journal
-- type name.
WITH page_journals AS (
    SELECT DISTINCT j.id
    FROM journal_entries je
    JOIN journals j ON j.id = je.journal_id
    JOIN classifications pc ON pc.id = je.classification_id
    WHERE je.account_holder = $1
      AND pc.balance_role <> ''
      AND (sqlc.arg(cursor_id)::bigint = 0 OR j.id < sqlc.arg(cursor_id)::bigint)
    ORDER BY j.id DESC
    LIMIT sqlc.arg(page_limit)::bigint
)
SELECT
    j.id   AS journal_id,
    j.uid  AS journal_uid,
    jt.code AS kind,
    CASE
        WHEN COUNT(DISTINCT c.id) = 1 AND MAX(c.display_label) <> '' THEN MAX(c.display_label)
        WHEN jt.display_label <> '' THEN jt.display_label
        ELSE jt.name
    END::text AS kind_label,
    cur.uid  AS currency_uid,
    cur.code AS currency_code,
    SUM(CASE WHEN je.entry_type = c.normal_side THEN je.amount ELSE -je.amount END)::NUMERIC(30,18) AS net_amount,
    j.effective_at,
    (COALESCE(rj.uid::text, ''))::text AS reversal_of_uid,
    (COALESCE(j.metadata->>'memo', ''))::text AS memo
FROM journal_entries je
JOIN page_journals pj ON pj.id = je.journal_id
JOIN journals j        ON j.id = je.journal_id
LEFT JOIN journals rj  ON rj.id = j.reversal_of
JOIN journal_types jt  ON jt.id = j.journal_type_id
JOIN classifications c ON c.id = je.classification_id
JOIN currencies cur    ON cur.id = je.currency_id
WHERE je.account_holder = $1
  AND c.balance_role <> ''
GROUP BY j.id, j.uid, jt.id, cur.id, rj.uid
ORDER BY j.id DESC, cur.code;

-- name: ListHolderHolds :many
-- Outstanding reservation holds for the holder, newest first. Same hold
-- semantics as SumActiveReservations: 'active' holds the full reserved
-- amount, 'settling' holds the unsettled remainder.
SELECT
    r.uid,
    (CASE WHEN r.status = 'active' THEN r.reserved_amount
          ELSE r.reserved_amount - COALESCE(r.settled_amount, 0)
     END)::NUMERIC(30,18) AS held_amount,
    cur.uid  AS currency_uid,
    cur.code AS currency_code,
    r.created_at,
    r.expires_at
FROM reservations r
JOIN currencies cur ON cur.id = r.currency_id
WHERE r.account_holder = $1 AND r.status IN ('active', 'settling')
ORDER BY r.id DESC;

-- name: ListHolderCurrencies :many
-- Every currency the holder has ever touched (any journal entry implies a
-- balance history). Feeds the per-currency BalanceBreakdown fan-out.
SELECT DISTINCT cur.uid, cur.code
FROM journal_entries je
JOIN currencies cur ON cur.id = je.currency_id
JOIN classifications c ON c.id = je.classification_id
WHERE je.account_holder = $1
  AND c.balance_role <> ''
ORDER BY cur.code;
