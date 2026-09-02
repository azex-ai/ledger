-- Removes the owner-only unlink path and restores migration 006's
-- unconditional set-once guards on events.journal_id / bookings.journal_id.
--
-- ⚠️ Must run while the two guard functions are owned by the credential
-- issuing this file, or from a superuser connection -- the same fence 019's,
-- 020's and 024's down scripts carry.

DROP FUNCTION IF EXISTS ledger_unlink_event_journal(uuid);

CREATE OR REPLACE FUNCTION ledger_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['delivery_status', 'attempts', 'next_attempt_at', 'delivered_at', 'journal_id'];
BEGIN
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on events may only change %, and this statement changed something else',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    IF OLD.journal_id IS NOT NULL AND NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'ledger: events.journal_id is set-once and already set to %', OLD.journal_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION ledger_bookings_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['status', 'channel_ref', 'settled_amount', 'journal_id', 'metadata', 'updated_at'];
BEGIN
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on bookings may only change %, and this statement changed something else',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.settled_amount < OLD.settled_amount THEN
        RAISE EXCEPTION 'ledger: bookings.settled_amount must not decrease (% -> %)', OLD.settled_amount, NEW.settled_amount
            USING ERRCODE = 'check_violation';
    END IF;

    IF OLD.journal_id IS NOT NULL AND NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'ledger: bookings.journal_id is set-once and already set to %', OLD.journal_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

DROP FUNCTION IF EXISTS ledger_journal_unlink_is_authorized();
