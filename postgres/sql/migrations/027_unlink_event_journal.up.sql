-- Wave 3 adversarial re-review, 2026-09-02 (w3-review/money-path.md M-3;
-- remediation contract §7.15, option (A)).
--
-- ####  The wedge  ####
--
-- I-51 rule 4 requires a journal claiming an `event_uid` to touch the
-- booking's (account_holder, currency). That is the weakest rule that makes
-- the claim mean something, and it was chosen deliberately: amounts and
-- classifications legitimately vary across fees, spreads and multi-entry
-- settlements. For a credential holding write scope it is also no obstacle --
-- holder and currency are exactly what it already knows. Measured:
--
--     WEDGE squatter journal ... claimed event ... (0.01, unrelated classification)
--     WEDGE real settling journal: event "..." is already linked to a journal: conflict
--     WEDGE bookings.journal_id is now permanently 1 (the squatter's)
--
-- After that 0.01 journal, this booking's real accounting can never be
-- recorded: `events.journal_id` and `bookings.journal_id` are both set-once
-- (006), journals are append-only so the squatter cannot be removed, and the
-- library shipped no unlink path of any kind. The deposit/settlement pipeline
-- stops for that booking with no operator recourse.
--
-- Fail-closed with no door is not fail-closed; it is stuck. This adds the
-- door.
--
-- ####  Where the door is  ####
--
-- Owner-only, audited, and shut to `ledger_app`. The reasoning is the same
-- one 020 used for the audit tables and 024 for the anchor memory: a repair
-- capability in the hands of the credential the threat model assumes is
-- leaked is not a repair capability, it is the attack. If `ledger_app` could
-- unlink, I-51's set-once rule would be advisory -- a squatter could unlink
-- and re-squat at will, and a real settlement could be displaced after the
-- fact.
--
-- Two layers, because either alone is insufficient:
--
--   1. `ledger_unlink_event_journal(uuid)` is SECURITY DEFINER, owned by
--      `ledger_owner`, with EXECUTE revoked from PUBLIC. `ledger_app` gets
--      42501 (pinned).
--   2. The guards themselves (`ledger_events_guard`,
--      `ledger_bookings_guard`) grow ONE narrow exception: `journal_id` may
--      go non-NULL -> NULL when the caller holds `ledger_owner` AND a
--      transaction-local flag is set. Both halves are required. The flag
--      alone would be worthless -- `set_config` is available to any role --
--      and the role check alone would silently widen the set-once rule for
--      every owner-issued statement, including a mistaken one.
--
-- Deliberately NOT done: `ALTER TABLE ... DISABLE TRIGGER` around the update
-- (016's shape). That takes ACCESS EXCLUSIVE on `bookings` and `events` --
-- the two hottest tables in the schema, stalling readers -- and while it is
-- held the guard is off for the whole table rather than for one statement.
-- A named exception inside the guard is narrower and lock-free.
--
-- ####  What the door does not do  ####
--
-- The squatter journal is left exactly as it is. Journals are append-only,
-- the row is true (it was posted, it did claim the event), and it is the
-- forensic record of the incident. Its `journals.event_id` therefore still
-- points at the event afterwards, so the event may end up with one stale
-- claimant and one real one. Nothing reads journals by `event_id` (no query
-- in postgres/sql/queries/ selects on it), and `event_id` is not part of the
-- canonical journal digest (core/auth.go), so no signature is affected.
-- Clearing it would have meant rewriting an append-only row to make the
-- history tidier, which is the one thing this library never does.
--
-- Reversing the squatter's accounting, if it moved money, is a separate and
-- ordinary act: post a reversal (I-51). This function only reopens the link.

------------------------------------------------------------
-- 1. One named exception in the two set-once guards.
------------------------------------------------------------

-- Shared predicate, so the two guards cannot drift apart. `pg_has_role(...,
-- 'USAGE')` rather than `current_user = 'ledger_owner'`: inside the SECURITY
-- DEFINER function current_user IS ledger_owner, a superuser (the migration
-- runner in tests, and a DBA in an incident) also passes, and ledger_app --
-- which holds no membership -- does not.
CREATE FUNCTION ledger_journal_unlink_is_authorized() RETURNS boolean
LANGUAGE sql STABLE
SET search_path = public, pg_temp
AS $$
    SELECT current_setting('ledger.unlink_event_journal', true) = 'on'
       AND pg_has_role(current_user, 'ledger_owner', 'USAGE');
$$;

ALTER FUNCTION ledger_journal_unlink_is_authorized() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_journal_unlink_is_authorized() FROM PUBLIC;

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
        -- Migration 027: the one exception. Clearing the link (never moving
        -- it to another journal) is permitted to ledger_owner inside
        -- ledger_unlink_event_journal, which sets the flag transaction-locally.
        IF NOT (NEW.journal_id IS NULL AND ledger_journal_unlink_is_authorized()) THEN
            RAISE EXCEPTION 'ledger: events.journal_id is set-once and already set to %', OLD.journal_id
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_events_guard() OWNER TO ledger_owner;

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
        IF NOT (NEW.journal_id IS NULL AND ledger_journal_unlink_is_authorized()) THEN
            RAISE EXCEPTION 'ledger: bookings.journal_id is set-once and already set to %', OLD.journal_id
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_bookings_guard() OWNER TO ledger_owner;

------------------------------------------------------------
-- 2. The door itself.
------------------------------------------------------------

CREATE FUNCTION ledger_unlink_event_journal(p_event_uid uuid) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_event_id        BIGINT;
    v_event_journal   BIGINT;
    v_booking_id      BIGINT;
    v_booking_journal BIGINT;
BEGIN
    SELECT id, journal_id, booking_id
      INTO v_event_id, v_event_journal, v_booking_id
      FROM events
     WHERE uid = p_event_uid;

    IF v_event_id IS NULL THEN
        RAISE EXCEPTION 'ledger: no event with uid %', p_event_uid
            USING ERRCODE = 'no_data_found';
    END IF;

    IF v_event_journal IS NULL THEN
        -- Never silent: a runbook step that reports success having done
        -- nothing is exactly the failure mode this repo keeps finding
        -- (working-agreements.md §3).
        RAISE EXCEPTION 'ledger: event % carries no journal link, so there is nothing to unlink', p_event_uid
            USING ERRCODE = 'no_data_found';
    END IF;

    PERFORM set_config('ledger.unlink_event_journal', 'on', true);

    UPDATE events SET journal_id = NULL WHERE id = v_event_id;

    IF v_booking_id IS NOT NULL AND v_booking_id <> 0 THEN
        SELECT journal_id INTO v_booking_journal FROM bookings WHERE id = v_booking_id FOR UPDATE;
        -- Only when it is the SAME journal. A booking whose link was set by
        -- some other transition is not this event's to reopen, and clearing
        -- it would destroy a correct link on the strength of an unrelated
        -- repair.
        IF v_booking_journal IS NOT NULL AND v_booking_journal = v_event_journal THEN
            UPDATE bookings SET journal_id = NULL, updated_at = now() WHERE id = v_booking_id;
        END IF;
    END IF;

    -- The forensic row. 020's AFTER UPDATE triggers on events and bookings
    -- record the raw before/after of each row on their own; this one records
    -- the OPERATION -- which event, which journal, which booking, and which
    -- authenticated role asked -- so an incident reader does not have to
    -- reconstruct intent from two unrelated column diffs.
    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (
        'ledger_unlink_event_journal',
        jsonb_build_object('event_uid', p_event_uid, 'event_journal_id', v_event_journal,
                           'booking_id', v_booking_id, 'booking_journal_id', v_booking_journal),
        jsonb_build_object('event_uid', p_event_uid, 'event_journal_id', NULL::bigint,
                           'booking_id', v_booking_id, 'booking_journal_id', NULL::bigint),
        session_user
    );

    PERFORM set_config('ledger.unlink_event_journal', 'off', true);
END;
$$;

ALTER FUNCTION ledger_unlink_event_journal(uuid) OWNER TO ledger_owner;

-- No GRANT to ledger_app, deliberately and permanently: see this file's
-- header. The owner reaches it as owner; PUBLIC reaches nothing.
REVOKE ALL ON FUNCTION ledger_unlink_event_journal(uuid) FROM PUBLIC;
