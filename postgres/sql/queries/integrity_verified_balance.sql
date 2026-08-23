-- integrity_verified_balance.sql
--
-- Wave 2, T2 of the integrity-hardening effort: the query backing
-- core.VerifiedBalanceReader / postgres.VerifiedBalanceStore.
--
-- See docs/plans/2026-08-21-integrity-hardening-contracts.md §W2-1/§W2-2 and
-- docs/plans/2026-08-21-tamper-evident-ledger-design.md §7.

-- name: ListContributingJournalIDs :many
-- Every distinct journal_id that posted at least one entry to
-- (account_holder, currency_id, classification_id) -- the exact set
-- VerifiedBalanceStore.VerifiedBalance must authorization-check
-- (core.VerifyJournalAuth) before it can trust
-- RecomputeCheckpointFromEntries' sum for this dimension. Excluding an
-- unauthorized journal from that sum instead of refusing to answer could
-- report a balance HIGHER than the true one (a reversal's net contribution
-- can be negative) -- contracts §W2-1. Ordered by id purely for
-- deterministic test output; verification order does not matter since
-- ANY single failure makes the whole result UNDEFINED.
SELECT DISTINCT journal_id
FROM journal_entries
WHERE account_holder = sqlc.arg(account_holder)::bigint
  AND currency_id = sqlc.arg(currency_id)::bigint
  AND classification_id = sqlc.arg(classification_id)::bigint
ORDER BY journal_id;
