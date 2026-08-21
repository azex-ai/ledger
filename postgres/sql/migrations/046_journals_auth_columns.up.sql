-- 046_journals_auth_columns.up.sql
--
-- P5 of the integrity-hardening wave
-- (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7,
-- docs/plans/2026-08-21-integrity-hardening-contracts.md §2/§4/§7): add the
-- per-journal KMS authorization signature columns. Expand-safe (NOT NULL
-- DEFAULT '' / ''::bytea) -- every pre-existing row and every consumer that
-- never wires a core.Attestor is unaffected (design doc §12 P5 row).
--
--   auth_digest    -- the canonical uid-space digest that was signed
--                     (core.CanonicalJournalDigest's output)
--   auth_signature -- the Attestor's signature over auth_digest
--   auth_key_id    -- the KMS key version that produced auth_signature,
--                     so key rotation can be tracked per journal
--
-- Empty auth_key_id ('') means "this journal was never signed" -- either no
-- Attestor was configured, or the store was tx-bound (postgres.LedgerStore's
-- PostJournal only signs in pool mode -- financial.md forbids external
-- (KMS) calls inside a DB transaction, and a tx-bound store is always
-- inside one it did not open). Nothing in this migration enforces "some
-- journals must be signed" -- that is I-26's job, checked by a downstream
-- consumer (core.VerifyJournalAuth), not a DB constraint.
------------------------------------------------------------
ALTER TABLE journals
    ADD COLUMN IF NOT EXISTS auth_digest    BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS auth_signature BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS auth_key_id    TEXT  NOT NULL DEFAULT '';

------------------------------------------------------------
-- Rebuild the anti-tamper guard (033's rule: "any migration that adds a
-- column to journals MUST also recreate this function with the column
-- included").
--
-- ⚠️ Integration note for whoever merges this alongside P4 (045,
-- ledger_journals_block_arbitrary_update()'s next scheduled edit per
-- contracts §2's evolution table): as of this migration's authoring,
-- migration 045 (P4's event_id set-once guard) had not yet landed on
-- `main`, so this function body is built from 033's CURRENT protected-column
-- list (id, journal_type_id, idempotency_key, total_debit, total_credit,
-- metadata, actor_id, source, reversal_of, created_at, effective_at, uid)
-- plus this migration's three new columns -- it deliberately does NOT
-- protect event_id. Because golang-migrate applies migrations in strict
-- numeric order regardless of git merge order, 045 will always run before
-- 046 against any real database (045 < 046) -- so if 045 lands with its own
-- CREATE OR REPLACE of this same function, 045's version will be
-- transiently correct and then immediately superseded by this file's
-- CREATE OR REPLACE, which does NOT know about event_id. That would
-- silently regress A4's protection. Team Lead: when merging 045, either (a)
-- ensure 046 (this file) is updated to fold in 045's event_id column +
-- semantics before the final merge, or (b) bump 045 to run after 046 is
-- authored with full knowledge of 045's exact predicate. Flagged via
-- `bus send team-lead` at delivery time -- do not resolve silently by
-- guessing 045's not-yet-written implementation.
------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_journals_block_arbitrary_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id              IS DISTINCT FROM OLD.id              OR
       NEW.journal_type_id IS DISTINCT FROM OLD.journal_type_id OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.total_debit     IS DISTINCT FROM OLD.total_debit     OR
       NEW.total_credit    IS DISTINCT FROM OLD.total_credit    OR
       NEW.metadata        IS DISTINCT FROM OLD.metadata        OR
       NEW.actor_id        IS DISTINCT FROM OLD.actor_id        OR
       NEW.source          IS DISTINCT FROM OLD.source          OR
       NEW.reversal_of     IS DISTINCT FROM OLD.reversal_of     OR
       NEW.created_at      IS DISTINCT FROM OLD.created_at      OR
       NEW.effective_at    IS DISTINCT FROM OLD.effective_at    OR
       NEW.uid             IS DISTINCT FROM OLD.uid             OR
       NEW.auth_digest     IS DISTINCT FROM OLD.auth_digest     OR
       NEW.auth_signature  IS DISTINCT FROM OLD.auth_signature  OR
       NEW.auth_key_id     IS DISTINCT FROM OLD.auth_key_id THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
