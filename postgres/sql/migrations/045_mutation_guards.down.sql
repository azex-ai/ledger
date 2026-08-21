-- 045_mutation_guards.down.sql
-- Reverses 045 in the opposite order it was applied.

-- A4: restore the 033 version of the journals guard (no event_id clause),
-- then revert event_id to its pre-045 NOT NULL DEFAULT 0 sentinel shape.
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

ALTER TABLE journals DROP CONSTRAINT IF EXISTS journals_event_id_fkey;
DROP INDEX IF EXISTS idx_journals_event;
UPDATE journals SET event_id = 0 WHERE event_id IS NULL;
ALTER TABLE journals ALTER COLUMN event_id SET NOT NULL;
ALTER TABLE journals ALTER COLUMN event_id SET DEFAULT 0;
CREATE INDEX idx_journals_event ON journals (event_id) WHERE event_id != 0;

-- A5
DROP TRIGGER IF EXISTS period_closes_no_delete ON period_closes;
DROP TRIGGER IF EXISTS period_closes_no_update ON period_closes;

-- A3
DROP TRIGGER IF EXISTS reservations_mutation_guard ON reservations;
DROP FUNCTION IF EXISTS ledger_reservations_guard();

-- A1 + A2
DROP TRIGGER IF EXISTS classifications_mutation_guard ON classifications;
DROP FUNCTION IF EXISTS ledger_classifications_guard();
