-- P5 (per-journal authorization signing) reads. See
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §3 -- this file is
-- exclusively owned by P5; new P5 queries land here, not in journals.sql.

-- name: GetJournalAuthByUID :one
-- Returns the stored signature material for a journal, keyed by its
-- external uid. Used by core.VerifyJournalAuth-based checks (pin tests
-- today; a future withdrawal gate / reconcile check / ledger-cli verify are
-- explicitly NOT wired by this phase, design doc §7.3/§7.4/§12).
SELECT uid, journal_type_id, idempotency_key, actor_id, source, reversal_of,
       event_id, effective_at, auth_digest, auth_signature, auth_key_id
FROM journals
WHERE uid = $1;
