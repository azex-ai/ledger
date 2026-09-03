-- name: CorruptReversalLinks :many
-- Fleet-wide scan for reversal chains that are not reversal chains
-- (docs/INVARIANTS.md I-51; 2026-09-03 independent review, money-out.md M-2).
--
-- `journals.reversal_of` is an ordinary column and `ledger_app` holds INSERT
-- on `journals`, so I-51's rules -- which run on the way in through this
-- library, and again where a reversal is DERIVED -- say nothing about a row
-- appended directly. Both of those are triggered by somebody trying to post
-- or reverse something; until one of them fires, a forged link sits in the
-- table unreported. This is the check that looks without being asked.
--
-- Two violations, one row each, mirroring the two rules that can be checked
-- against data alone:
--
--   unmatched_dimension -- an entry of a journal linked to O that, flipped
--     back onto O's grain, lands on a (holder, currency, classification,
--     entry_type) dimension O never posted. That is what the net-zero
--     forgery looks like: legs on both sides of a real dimension, so the
--     money does not move and the "already reversed" figure still climbs.
--
--   over_reversed -- the cumulative amount linked to O on one dimension
--     exceeds what O posted there. Reported per (original, dimension)
--     rather than per journal: an overshoot is a property of the total, and
--     blaming whichever journal happens to be last points at the wrong row.
--     reversal_uid is empty for these; every contributing journal is found
--     by the query in docs/RUNBOOK.md section 19.
--
-- uids, never internal ids, in the two identifier columns (I-18: this
-- report is returned verbatim by POST /reconcile/full). currency_id and
-- classification_id ARE internal ids and are resolved to uid/code by the
-- service layer before they reach a Finding, the same way every other
-- dimension-bearing check in the suite does it.
--
-- Bounded by $1 so a database with a large corrupt chain cannot make a
-- reconciliation run unbounded; hitting the limit marks the check
-- incomplete rather than truncating silently.
WITH rev_entries AS (
    SELECT r.id                 AS reversal_id,
           r.uid::text          AS reversal_uid,
           r.reversal_of        AS original_id,
           e.account_holder     AS account_holder,
           e.currency_id        AS currency_id,
           e.classification_id  AS classification_id,
           CASE WHEN e.entry_type = 'debit' THEN 'credit' ELSE 'debit' END AS original_entry_type,
           e.amount             AS amount
    FROM journals r
    JOIN journal_entries e ON e.journal_id = r.id
    WHERE r.reversal_of IS NOT NULL
),
orig_dims AS (
    SELECT e.journal_id        AS original_id,
           e.account_holder    AS account_holder,
           e.currency_id       AS currency_id,
           e.classification_id AS classification_id,
           e.entry_type        AS entry_type,
           SUM(e.amount)       AS original_amount
    FROM journal_entries e
    WHERE EXISTS (SELECT 1 FROM rev_entries re WHERE re.original_id = e.journal_id)
    GROUP BY 1, 2, 3, 4, 5
),
unmatched AS (
    SELECT o.uid::text            AS original_uid,
           re.reversal_uid        AS reversal_uid,
           'unmatched_dimension'  AS violation,
           re.account_holder      AS account_holder,
           re.currency_id         AS currency_id,
           re.classification_id   AS classification_id,
           re.original_entry_type AS entry_type,
           SUM(re.amount)::numeric(30, 18) AS reversed_amount,
           0::numeric(30, 18)     AS original_amount
    FROM rev_entries re
    JOIN journals o ON o.id = re.original_id
    WHERE NOT EXISTS (
        SELECT 1 FROM orig_dims od
        WHERE od.original_id = re.original_id
          AND od.account_holder = re.account_holder
          AND od.currency_id = re.currency_id
          AND od.classification_id = re.classification_id
          AND od.entry_type = re.original_entry_type
    )
    GROUP BY 1, 2, 3, 4, 5, 6, 7
),
over_reversed AS (
    SELECT o.uid::text        AS original_uid,
           ''                 AS reversal_uid,
           'over_reversed'    AS violation,
           od.account_holder  AS account_holder,
           od.currency_id     AS currency_id,
           od.classification_id AS classification_id,
           od.entry_type      AS entry_type,
           SUM(re.amount)::numeric(30, 18) AS reversed_amount,
           od.original_amount::numeric(30, 18) AS original_amount
    FROM orig_dims od
    JOIN rev_entries re
      ON re.original_id = od.original_id
     AND re.account_holder = od.account_holder
     AND re.currency_id = od.currency_id
     AND re.classification_id = od.classification_id
     AND re.original_entry_type = od.entry_type
    JOIN journals o ON o.id = od.original_id
    GROUP BY 1, 2, 3, 4, 5, 6, 7, od.original_amount
    HAVING SUM(re.amount) > od.original_amount
)
SELECT original_uid, reversal_uid, violation, account_holder, currency_id, classification_id, entry_type, reversed_amount, original_amount
FROM (
    SELECT * FROM unmatched
    UNION ALL
    SELECT * FROM over_reversed
) AS violations
ORDER BY original_uid, violation, reversal_uid, account_holder, currency_id, classification_id, entry_type
LIMIT $1;
