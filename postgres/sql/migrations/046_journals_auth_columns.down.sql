-- Restore the 033 version of the guard (without the auth columns), then
-- drop the columns themselves.
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

ALTER TABLE journals
    DROP COLUMN IF EXISTS auth_digest,
    DROP COLUMN IF EXISTS auth_signature,
    DROP COLUMN IF EXISTS auth_key_id;
