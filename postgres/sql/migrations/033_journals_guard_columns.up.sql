-- Close a gap in the journals anti-tamper guard: migration 018's
-- ledger_journals_block_arbitrary_update() hardcodes the protected-column
-- list, and the columns added later — effective_at (025) and uid (031) —
-- were never added to it. Until now `UPDATE journals SET effective_at = ...`
-- silently bypassed the guard, letting a script move a posted journal into a
-- closed accounting period (I-15 only checks at post time) or rewrite the
-- external uid, with zero database-level defense.
--
-- Rule going forward (as of this migration): any migration that adds a
-- column to journals MUST also recreate this function with the column
-- included, unless the column is genuinely meant to be mutable post-insert
-- (like event_id's set-once backfill) -- there is no WHEN clause here or
-- anywhere in this function; that mutability, if wanted, has to be
-- implemented in the function body.
--
-- UPDATE (migration 045, docs/plans/2026-08-21-integrity-hardening-contracts.md
-- §2): this per-migration hardcoded column list turned out to be exactly
-- the kind of rule that gets silently broken -- p5-authsig found that two
-- migrations each issuing their own CREATE OR REPLACE with their own
-- hardcoded list means the later one drops the earlier one's column
-- protection. 045 replaces this function with a generic to_jsonb(OLD)
-- vs to_jsonb(NEW) comparison against an explicit mutable-column whitelist,
-- so this comment's "rule going forward" no longer applies: any future
-- column is protected by default, and only 045 needs to be touched again.
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
       NEW.uid             IS DISTINCT FROM OLD.uid THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
