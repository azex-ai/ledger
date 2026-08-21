-- 046_journals_auth_columns.up.sql
--
-- P5 of the integrity-hardening wave
-- (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7,
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §2/§4/§7): add the
-- per-journal authorization signature columns. Expand-safe (NOT NULL
-- DEFAULT '' / ''::bytea) -- every pre-existing row and every consumer that
-- never wires a core.Attestor is unaffected (design doc §12 P5 row).
--
--   auth_digest    -- the canonical uid-space digest that was signed
--                     (core.CanonicalJournalDigest's output)
--   auth_signature -- the Attestor's signature over auth_digest
--   auth_key_id    -- the signing key's identifier, so key rotation can be
--                     tracked per journal
--
-- Empty auth_key_id ('') means "this journal was never signed" -- either no
-- Attestor was configured, or the store was tx-bound (postgres.LedgerStore's
-- PostJournal only signs in pool mode -- financial.md forbids external
-- calls inside a DB transaction, and a tx-bound store is always inside one
-- it did not open). Nothing in this migration enforces "some journals must
-- be signed" -- that is I-26's job, checked by a downstream consumer
-- (core.VerifyJournalAuth), not a DB constraint.
--
-- This migration deliberately does NOT touch
-- ledger_journals_block_arbitrary_update(): contracts §2 (2026-08-21
-- rewrite) replaced that function's hardcoded per-migration column list
-- with a generic to_jsonb(OLD)/to_jsonb(NEW) comparison against an
-- explicit mutable-column whitelist, so newly added columns (these three
-- included) are protected automatically -- there is nothing for this
-- migration to add to that function. Only migration 045 (P4) touches it
-- (installing the generic version + the event_id set-once exception).
------------------------------------------------------------
ALTER TABLE journals
    ADD COLUMN IF NOT EXISTS auth_digest    BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS auth_signature BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS auth_key_id    TEXT  NOT NULL DEFAULT '';
