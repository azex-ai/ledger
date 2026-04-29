-- name: GetJournalTypeIDByCode :one
-- Returns the journal_type_id for the given code.  Used by PendingStore to
-- resolve template journal types without a round-trip through the template engine.
SELECT id FROM journal_types WHERE code = $1 AND is_active = TRUE;

-- name: ListPendingJournalsOlderThan :many
-- Returns at most 1000 "deposit_pending" journals created before :cutoff whose
-- account still has a positive pending-classification balance.  Uses a subquery
-- so the balance check and the journal scan share the same snapshot.
--
-- NOTE: classification_id is passed as a parameter because the store resolves
-- it by code at startup; it is not hard-coded here to stay schema-agnostic.
SELECT DISTINCT ON (je.account_holder, je.currency_id)
    j.id              AS journal_id,
    je.account_holder,
    je.currency_id,
    j.total_debit     AS amount
FROM journals j
JOIN journal_types jt ON jt.id = j.journal_type_id AND jt.code = 'deposit_pending'
JOIN journal_entries je ON je.journal_id = j.id
    AND je.classification_id = sqlc.arg(pending_classification_id)::bigint
    AND je.entry_type = 'credit'
WHERE j.created_at < sqlc.arg(cutoff)::timestamptz
ORDER BY je.account_holder, je.currency_id, j.id ASC
LIMIT 1000;
