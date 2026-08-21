-- 045_mutation_guards.up.sql
--
-- Close the five mutation-guard gaps identified in the tamper-evident-ledger
-- design (docs/plans/2026-08-21-tamper-evident-ledger-design.md §6, A1-A5):
-- every non-journal table that participates in balance computation was
-- missing a DB-level guard against post-insert mutation, so a writer with
-- app DB credentials could change a holder's effective balance without
-- ever touching journal_entries.
--
-- Depends on P1 (migration 042): without role separation these triggers can
-- simply be DROPped by the same credential that would abuse the columns
-- they protect (contract §6).

------------------------------------------------------------
-- A1 + A2: classifications.normal_side / classifications.balance_role.
--
-- normal_side (002) has no legitimate post-insert mutation path anywhere in
-- the codebase -- it is immutable by convention only, enforced nowhere.
-- balance_role (032) has one legitimate path, ClassificationStore.SetBalanceRole
-- (postgres/classification_store.go), documented as an expand-style upgrade:
-- '' -> <role>. That is the ONLY transition the guard allows; switching
-- between two non-empty roles, or reverting to '', silently re-buckets the
-- holder-facing available/pending/locked breakdown with no accounting event
-- and is exactly what SetBalanceRole's own doc comment says callers must not
-- do casually.
--
-- Every other classifications column (code, name, is_system, is_active,
-- created_at, lifecycle, display_label, uid) already has a legitimate
-- mutation path (DeactivateClassification / SetLifecycleIfEmpty /
-- SetDisplayLabelIfEmpty) and is left alone.
------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_classifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.normal_side IS DISTINCT FROM OLD.normal_side THEN
        RAISE EXCEPTION 'ledger: classifications.normal_side is immutable; it determines the sign of every historical rollup for this classification'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_role IS DISTINCT FROM OLD.balance_role AND OLD.balance_role <> '' THEN
        RAISE EXCEPTION 'ledger: classifications.balance_role is already set to %; only the '''' -> <role> upgrade is allowed', OLD.balance_role
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS classifications_mutation_guard ON classifications;
CREATE TRIGGER classifications_mutation_guard
    BEFORE UPDATE ON classifications
    FOR EACH ROW EXECUTE FUNCTION ledger_classifications_guard();

------------------------------------------------------------
-- A3: reservations.
--
-- account_holder/currency_id/reserved_amount/idempotency_key/expires_at/
-- created_at/uid have no legitimate mutation path (InsertReservation is the
-- only writer of any of them) and are immutable.
--
-- settled_amount only ever accumulates (SettlePartial's own precondition
-- already guarantees this; the guard makes it a DB-level fact rather than a
-- caller convention -- a decrease can only be tampering).
--
-- journal_id is set-once: NULL -> non-NULL is the only legal transition,
-- matching the FK-target-exception shape used for bookings.journal_id /
-- events.journal_id (018) / reservations.journal_id's own FK (035).
--
-- status follows the whitelist state machine in core/reserve.go
-- (reservationTransitions): active -> {settling, settled, released},
-- settling -> {settled, released}. settled/released are terminal.
------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_reservations_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.account_holder  IS DISTINCT FROM OLD.account_holder  OR
       NEW.currency_id     IS DISTINCT FROM OLD.currency_id     OR
       NEW.reserved_amount IS DISTINCT FROM OLD.reserved_amount OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.expires_at      IS DISTINCT FROM OLD.expires_at      OR
       NEW.created_at      IS DISTINCT FROM OLD.created_at      OR
       NEW.uid             IS DISTINCT FROM OLD.uid THEN
        RAISE EXCEPTION 'ledger: UPDATE on reservations may not change account_holder/currency_id/reserved_amount/idempotency_key/expires_at/created_at/uid'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.settled_amount < OLD.settled_amount THEN
        RAISE EXCEPTION 'ledger: reservations.settled_amount must not decrease (% -> %)', OLD.settled_amount, NEW.settled_amount
            USING ERRCODE = 'check_violation';
    END IF;

    IF OLD.journal_id IS NOT NULL AND NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'ledger: reservations.journal_id is set-once and already set to %', OLD.journal_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'active'   AND NEW.status IN ('settling', 'settled', 'released')) OR
            (OLD.status = 'settling' AND NEW.status IN ('settled', 'released'))
        ) THEN
            RAISE EXCEPTION 'ledger: reservations status transition % -> % is not allowed', OLD.status, NEW.status
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS reservations_mutation_guard ON reservations;
CREATE TRIGGER reservations_mutation_guard
    BEFORE UPDATE ON reservations
    FOR EACH ROW EXECUTE FUNCTION ledger_reservations_guard();

------------------------------------------------------------
-- A5: period_closes. Documented as append-only (026) but never enforced.
-- Reuses ledger_block_mutation() from 018 (the same unconditional
-- "raise on any UPDATE/DELETE" function journal_entries uses) -- every
-- column on this table is identity/audit, there is no partial-mutation
-- case to carve out the way classifications/reservations need.
------------------------------------------------------------
DROP TRIGGER IF EXISTS period_closes_no_update ON period_closes;
CREATE TRIGGER period_closes_no_update
    BEFORE UPDATE ON period_closes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS period_closes_no_delete ON period_closes;
CREATE TRIGGER period_closes_no_delete
    BEFORE DELETE ON period_closes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

------------------------------------------------------------
-- A4: journals.event_id set-once + FK, AND the journals anti-tamper guard
-- becomes a generic comparison instead of a per-migration hardcoded column
-- list (contract §2, 2026-08-21 Team Lead ruling that overturned §2's
-- original per-migration-relay plan).
--
-- 014 added event_id as NOT NULL DEFAULT 0 (a sentinel, no FK). No app code
-- path ever UPDATEs it -- InsertJournal sets it once at insert time and
-- there is no "backfill" query anywhere in postgres/sql/queries/journals.sql
-- despite 018's and 033's trigger comments describing (but never
-- implementing) an event_id "set-once backfill" WHEN clause. Converting to
-- the same nullable FK-target-exception shape as reservations.journal_id
-- (035) both fixes the missing FK and gives the set-once semantics those
-- comments always claimed to enforce, for whatever future backfill path
-- needs it.
--
-- Why the guard function itself changes shape here: p5-authsig found that
-- 033's pattern -- hardcode the protected-column list, CREATE OR REPLACE it
-- in every migration that touches journals -- has a real ordering bug.
-- golang-migrate runs 045 then 046 in numeric order; if each issues its own
-- CREATE OR REPLACE with its own hardcoded list, 046's REPLACE silently
-- drops the event_id protection 045 just added. The root cause isn't
-- ordering, it's that 033 turned "remember to recreate this function
-- whenever you add a column" into a rule a human has to carry -- and that
-- rule had already been broken once (033 exists to repair the columns 025
-- and 031 forgot). Comparing to_jsonb(OLD)/to_jsonb(NEW) minus an explicit
-- mutable-column whitelist means any future column is protected by default
-- (fail-closed by construction) and only this one migration ever needs to
-- touch the function body again.
------------------------------------------------------------
ALTER TABLE journals ALTER COLUMN event_id DROP DEFAULT;
ALTER TABLE journals ALTER COLUMN event_id DROP NOT NULL;
UPDATE journals SET event_id = NULL WHERE event_id = 0;
ALTER TABLE journals
    ADD CONSTRAINT journals_event_id_fkey
    FOREIGN KEY (event_id) REFERENCES events(id);

DROP INDEX IF EXISTS idx_journals_event;
CREATE INDEX idx_journals_event ON journals (event_id) WHERE event_id IS NOT NULL;

CREATE OR REPLACE FUNCTION ledger_journals_block_arbitrary_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- The only columns allowed to change post-insert. Adding a column here
    -- is an explicit, reviewable decision; anything NOT in this array
    -- (including every column added by a future migration) is protected by
    -- default.
    mutable CONSTANT text[] := ARRAY['event_id'];
BEGIN
    -- event_id's set-once semantics: only a NULL -> non-NULL transition is
    -- legal. Implemented in the function body, not a trigger WHEN clause --
    -- 033's WHEN clause was only ever described in a comment, never
    -- written (018:137-140 is an unconditional BEFORE UPDATE FOR EACH ROW).
    IF OLD.event_id IS NOT NULL AND NEW.event_id IS DISTINCT FROM OLD.event_id THEN
        RAISE EXCEPTION 'ledger: journals.event_id is set-once and already set to %', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except the set-once event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;
