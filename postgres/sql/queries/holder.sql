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
-- the same "spendable money" scope as BalanceBreakdown. Holder-side trackers
-- that carry no spendable role (fee_expense, ...) are bookkeeping detail:
-- including them would net a fee charge to zero and hide it from the user.
--
-- That prediction came true on 2026-08-26 and this filter is the reason. The
-- M-4 fix retagged fee_expense from balance_role='' to 'memo' so solvency
-- would stop counting it as a liability, and updated the liability predicate
-- in platform_balances.sql — but not the three copies here, which still read
-- `<> ''`. 'memo' is not '', so fee_expense joined this aggregate, withdraw_fee's
-- two holder-side entries (+5 memo, -5 locked) netted to exactly zero, and
-- holder_store.go's `net.IsZero()` filter dropped the whole row: the user's
-- balance fell by 5 with no line in their statement to explain it
-- (2026-09-02 audit A-M3).
--
-- The predicate is therefore `balance_role NOT IN ('', 'memo')` and it is
-- the SAME predicate platform_balances.sql uses for "what counts as a
-- liability". Those two questions — what the platform owes, and what money
-- the holder can see — are answered by one expression on purpose. Any new
-- core.BalanceRole value has to be reviewed against both meanings at once;
-- postgres/sign_authority_gate_test.go fails the build if the four copies of
-- this predicate ever stop being character-for-character identical.
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
      AND pc.balance_role NOT IN ('', 'memo')
      AND (sqlc.arg(cursor_id)::bigint = 0 OR j.id < sqlc.arg(cursor_id)::bigint)
    ORDER BY j.id DESC
    LIMIT sqlc.arg(page_limit)::bigint
)
SELECT
    j.id   AS journal_id,
    j.uid  AS journal_uid,
    -- kind is journal_types.holder_kind (M-7 fix, docs/INVARIANTS.md I-44) —
    -- the third shape this field has had. It started as journal_types.code
    -- (e.g. "deposit_confirm"), an internal accounting-engine identifier an
    -- operator names when configuring their own journal types -- narrating
    -- *how the ledger produced the balance*, which the holder-facing
    -- surface must not do (~/.claude/rules/user-facing-surfaces.md; the
    -- same principle presets/devcredit.go documents for kind_label's
    -- DisplayLabel choice). A first fix switched it to journal_types.uid:
    -- compliant, but opaque and per-deployment-random, so a host app's
    -- kindLabels map (keyed by a literal it hardcodes) could never match
    -- it. holder_kind is a small, stable, deployment-independent product
    -- vocabulary (core.HolderTxKind: deposit/withdrawal/transfer/fee/
    -- adjustment/other) a journal type declares itself under once, that a
    -- host app CAN hardcode a kindLabels map against.
    --
    -- COALESCE(NULLIF(holder_kind, ''), 'other'): holder_kind is NOT NULL
    -- DEFAULT '' (migration 012) and '' is a legitimate stored value
    -- meaning "nobody has tagged this journal type yet" (see
    -- core.HolderTxKindNone's doc comment for why that is tolerated here,
    -- unlike classifications.balance_role after the M-4 fix). This read
    -- path never lets that internal "untagged" state leak onto the wire as
    -- an empty string, which is not a member of the HolderTxKind
    -- enum @azex/ledger-react's consumers switch on — an untagged journal
    -- type instead reads as the same 'other' bucket a journal type author
    -- who explicitly chose "none of the above" would produce. This is a
    -- disclosed, documented fallback, not a silent one — see this
    -- migration's header comment and docs/INVARIANTS.md I-44.
    COALESCE(NULLIF(jt.holder_kind, ''), 'other')::text AS kind,
    CASE
        WHEN COUNT(DISTINCT c.id) = 1 AND MAX(c.display_label) <> '' THEN MAX(c.display_label)
        WHEN jt.display_label <> '' THEN jt.display_label
        ELSE jt.name
    END::text AS kind_label,
    cur.uid  AS currency_uid,
    cur.code AS currency_code,
    SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount))::NUMERIC(30,18) AS net_amount,
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
  AND c.balance_role NOT IN ('', 'memo')
GROUP BY j.id, j.uid, jt.id, cur.id, rj.uid
ORDER BY j.id DESC, cur.code;

-- name: ListHolderHolds :many
-- Outstanding reservation holds for the holder, newest first. Same hold
-- semantics as SumActiveReservations: 'active' holds the full reserved
-- amount, 'settling' holds the unsettled remainder.
--
-- Keyset-paginated on r.id DESC (H-m9): this used to return the holder's
-- entire hold set with no LIMIT at all, so one holder with a runaway number
-- of active reservations produced an unbounded response body from an
-- unbounded scan. Same cursor shape and direction as page_journals above and
-- ListReservationsByAccount: cursor_id = 0 is the first page, and the caller
-- encodes the last row's id as the opaque next_cursor.
--
-- r.id (not created_at) is the cursor key: it is unique, so a page boundary
-- can never split or repeat a row that shares a timestamp with its
-- neighbour.
SELECT
    r.id,
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
  AND (sqlc.arg(cursor_id)::bigint = 0 OR r.id < sqlc.arg(cursor_id)::bigint)
ORDER BY r.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListHolderCurrencies :many
-- Every currency the holder has ever touched (any journal entry implies a
-- balance history). Feeds the per-currency BalanceBreakdown fan-out.
--
-- H-m9 (cost, documented rather than changed): this is a DISTINCT over the
-- holder's whole entry history. It resolves as an index-only scan on
-- idx_entries_account_id's (account_holder, currency_id) prefix, so it does
-- not read table pages, but it does read every index entry for the holder.
-- The obvious rewrite -- read the currency set out of balance_checkpoints,
-- which holds one row per dimension -- was considered and rejected as
-- written: a dimension with entries but no checkpoint row yet (rollup lag,
-- or a first write) is invisible there, and no cheap watermark makes the
-- fallback provably complete, because each dimension carries its own
-- last_entry_id. Silently dropping a currency from a holder wallet is a
-- worse defect than the scan, so the scan stays until there is a bounded
-- form that is provably complete. The RESULT is bounded by the deployment's
-- currency count, not by the holder's history length.
SELECT DISTINCT cur.uid, cur.code
FROM journal_entries je
JOIN currencies cur ON cur.id = je.currency_id
JOIN classifications c ON c.id = je.classification_id
WHERE je.account_holder = $1
  AND c.balance_role NOT IN ('', 'memo')
ORDER BY cur.code;
