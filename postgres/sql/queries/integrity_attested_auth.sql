-- T4 of the integrity-hardening wave (docs/plans/2026-08-21-integrity-hardening-contracts.md
-- §W3-0/§W3-B, docs/plans/2026-08-21-tamper-evident-ledger-design.md §8
-- extended). This file is exclusively owned by T4. It does NOT include the
-- ledger_attestations/entry_attestations INSERT queries -- those are
-- P6's existing InsertLedgerAttestation / InsertEntryAttestations in
-- integrity_attestations.sql, extended in place (the same way migration
-- 048/P7 extended them for merkle_root/leaf_hash) rather than duplicated
-- here.

-- name: ListJournalsForAuthCheck :many
-- Batch-fetches everything core.VerifyJournalAuth needs to reconstruct a
-- journal's uid-space core.JournalInput, for every id in journal_ids, in
-- one round trip (design doc §4.5's batched-fetch recommendation). The
-- naive VerifiedBalanceReader path measured in
-- .local/bench-verify-2026-08-23.md pays one journals-JOIN-journal_entries
-- round trip PER journal; this query (paired with
-- ListEntriesForJournals below) pays it once per attestation batch or once
-- per ledger-cli verify pass instead.
--
-- reversal_of resolves to its uid via a self-join (NULL for an original,
-- non-reversal journal) -- core.CanonicalJournalDigest needs
-- input.ReversalOfUID, never the internal id (design doc §7.2). event_id is
-- deliberately NOT selected: CanonicalJournalDigest never covers it
-- (core/auth.go's authDigestDomain doc comment) -- resolving it here would
-- only invite a caller to assume otherwise.
SELECT
    j.id,
    j.uid,
    jt.uid AS journal_type_uid,
    j.idempotency_key,
    j.actor_id,
    j.source,
    rev.uid AS reversal_of_uid,
    j.effective_at,
    j.auth_digest,
    j.auth_signature,
    j.auth_key_id,
    j.auth_status
FROM journals j
JOIN journal_types jt ON jt.id = j.journal_type_id
LEFT JOIN journals rev ON rev.id = j.reversal_of
WHERE j.id = ANY(sqlc.arg(journal_ids)::bigint[]);

-- name: ListEntriesForJournals :many
-- Every journal_entries row for any of journal_ids, in one round trip --
-- the second half of the batched fetch ListJournalsForAuthCheck starts
-- (design doc §4.5). Ordered by journal_id so the caller can group rows by
-- journal without a map lookup per row; callers must not assume any
-- particular id order WITHIN one journal's group beyond that.
SELECT id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at
FROM journal_entries
WHERE journal_id = ANY(sqlc.arg(journal_ids)::bigint[])
ORDER BY journal_id, id;

-- name: ListContributingEntryVerdicts :many
-- Every entry contributing to (account_holder, currency_id,
-- classification_id), alongside its cached T4 attestation verdict if one
-- exists. '' (core.JournalAuthVerdictUnknown) means either "not yet
-- attested" (the tail -- entry_attestations has no row for this entry_id)
-- or "attested before migration 054 existed" -- postgres.VerifiedBalanceStore
-- treats both identically: fall back to a live core.VerifyJournalAuth for
-- that entry's journal, exactly as if T4 did not exist (fail-closed, never
-- silently treated as authorized). Reuses idx_entries_account_id, the same
-- index integrity_verified_balance.sql's ListContributingJournalIDs already
-- relies on.
SELECT je.id AS entry_id, je.journal_id,
       COALESCE(ea.auth_verdict, '') AS verdict
FROM journal_entries je
LEFT JOIN entry_attestations ea ON ea.entry_id = je.id
WHERE je.account_holder = sqlc.arg(account_holder)::bigint
  AND je.currency_id = sqlc.arg(currency_id)::bigint
  AND je.classification_id = sqlc.arg(classification_id)::bigint;
