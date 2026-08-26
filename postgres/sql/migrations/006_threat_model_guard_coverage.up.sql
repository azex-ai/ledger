-- Close the gap the ACL-derivation loop in 001_baseline section 14 leaves
-- open by construction: it classifies a table as append-only only when it
-- carries a BEFORE UPDATE trigger executing exactly `ledger_block_mutation()`.
-- Every table with NO trigger at all -- not "declared safe", just never
-- looked at -- fell out the permissive side and kept a full UPDATE grant.
-- Seven tables were in that state and every one of them decides where money
-- goes or whether tampering with it can be seen:
--
--     account_policies          the only DB-enforced freeze/overdraft floor
--     account_policy_changes    its own audit trail
--     bookings                  journal_id documented as set-once, unguarded
--     events                    the outbound delivery record AND the
--                               booking-lifecycle log; amount/to_status/
--                               journal_id were all writable
--     reservation_settlement_legs
--     reservation_operation_receipts   idempotency receipts (board #25):
--     booking_transition_receipts      forging one short-circuits the
--                               matching Settle/Release/Transition call
--
-- Every UPDATE below was run against a real database as ledger_app, before
-- this migration was written, and every one succeeded. Evidence is in the
-- bus checkpoint for this task, not repeated here.
--
-- ####  Why whitelist guards here and blanket refusal there  ####
--
-- account_policies, bookings and events each have real, narrow, legitimate
-- mutation paths (SetPolicy, UpdateBookingTransition, delivery tracking) --
-- REVOKEing UPDATE outright would break them, so they get the same
-- generic-whitelist shape 003 introduced: to_jsonb(OLD) minus the allowed
-- columns compared against to_jsonb(NEW) minus the same, so a column a future
-- migration adds is refused by default instead of by omission.
--
-- account_policy_changes and the three receipt tables have no legitimate
-- UPDATE at all -- every one of them is written once by an INSERT and never
-- touched again by any query in postgres/sql/queries/ -- so they get the
-- blanket ledger_block_mutation() refusal plus REVOKE UPDATE, matching
-- entry_template_lines' treatment in 003.

CREATE OR REPLACE FUNCTION ledger_account_policies_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['status', 'min_balance', 'enforce_min_balance', 'note', 'updated_at'];
BEGIN
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on account_policies may only change %, and this statement changed something else -- account_holder/currency_id/classification_id identify the policy and status/min_balance/enforce_min_balance are the only enforcement knobs',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER account_policies_mutation_guard
    BEFORE UPDATE ON account_policies
    FOR EACH ROW EXECUTE FUNCTION ledger_account_policies_guard();

-- journal_id follows the same set-once rule reservations.journal_id already
-- enforces (section 12): a booking's lifecycle may have at most one
-- journal-bearing transition. status/channel_ref/settled_amount/metadata/
-- updated_at are UpdateBookingTransition's actual mutable set; everything
-- else -- classification_id, account_holder, currency_id, amount,
-- channel_name, reservation_id, idempotency_key, expires_at, created_at, uid
-- -- decides which classification's lifecycle this is or how much money it
-- moves, and stays fixed for the life of the row.
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

CREATE TRIGGER bookings_mutation_guard
    BEFORE UPDATE ON bookings
    FOR EACH ROW EXECUTE FUNCTION ledger_bookings_guard();

-- events is both the lifecycle-transition record and the outbound delivery
-- queue (delivery_status/attempts/next_attempt_at/delivered_at). Only the
-- delivery-tracking columns and the set-once journal_id backfill are
-- legitimately written after INSERT; from_status/to_status/amount/
-- settled_amount/account_holder/currency_id/classification_code/metadata/
-- occurred_at/actor_id/source describe what happened and must not move.
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

CREATE TRIGGER events_mutation_guard
    BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION ledger_events_guard();

-- account_policy_changes and the three idempotency-receipt tables: no query
-- in postgres/sql/queries/ ever UPDATEs any of them. Blanket refusal plus
-- REVOKE, exactly like entry_template_lines in 003.
CREATE TRIGGER account_policy_changes_no_update
    BEFORE UPDATE ON account_policy_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER account_policy_changes_no_delete
    BEFORE DELETE ON account_policy_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER reservation_settlement_legs_no_update
    BEFORE UPDATE ON reservation_settlement_legs
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER reservation_settlement_legs_no_delete
    BEFORE DELETE ON reservation_settlement_legs
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER reservation_operation_receipts_no_update
    BEFORE UPDATE ON reservation_operation_receipts
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER reservation_operation_receipts_no_delete
    BEFORE DELETE ON reservation_operation_receipts
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER booking_transition_receipts_no_update
    BEFORE UPDATE ON booking_transition_receipts
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER booking_transition_receipts_no_delete
    BEFORE DELETE ON booking_transition_receipts
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

REVOKE UPDATE ON public.account_policy_changes           FROM ledger_app;
REVOKE UPDATE ON public.reservation_settlement_legs       FROM ledger_app;
REVOKE UPDATE ON public.reservation_operation_receipts     FROM ledger_app;
REVOKE UPDATE ON public.booking_transition_receipts        FROM ledger_app;

-- ####  reconcile_scan_cursors: audit trail, not a mutation guard  ####
--
-- This table cannot take a whitelist or blanket guard: SetScanCursor
-- legitimately overwrites after_holder/after_currency/lap_dirty to ANY value,
-- including resetting to the start of a lap (service/reconcile.go:716) --
-- that is indistinguishable, from the DB's side, from the attack that sets
-- after_holder to the maximum int64 to make the next scan see zero rows and
-- report Complete=true. A leaked ledger_app credential can still do the
-- attack after this migration; that half of the fix (refusing to trust a
-- zero-row scan as complete) is service/reconcile.go, owned by a different
-- task per docs/plans/2026-08-26-audit-remediation-contracts.md §8's second
-- seam.
--
-- What a DB-level change CAN do unilaterally is make every cursor write --
-- legitimate or not -- leave a trace, closing the §9 gap (a rejected write
-- leaves no record; this is the opposite problem, a table with no guard at
-- all leaves no record either way). The AFTER trigger fires regardless of
-- who issued the UPDATE or why, so tampering cannot avoid being logged by
-- declining to also write the audit row the way an attacker with only
-- ledger_app's grants could skip an application-level audit INSERT.
CREATE TABLE reconcile_scan_cursor_changes (
    id                 BIGSERIAL PRIMARY KEY,
    check_name         TEXT NOT NULL,
    old_after_holder   BIGINT NOT NULL,
    old_after_currency BIGINT NOT NULL,
    old_lap_dirty      BOOLEAN NOT NULL,
    new_after_holder   BIGINT NOT NULL,
    new_after_currency BIGINT NOT NULL,
    new_lap_dirty      BOOLEAN NOT NULL,
    changed_by         TEXT NOT NULL DEFAULT current_user,
    changed_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_reconcile_scan_cursor_changes_check ON reconcile_scan_cursor_changes (check_name, changed_at DESC);

CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty,
        new_after_holder, new_after_currency, new_lap_dirty
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty
    );
    RETURN NEW;
END;
$$;

CREATE TRIGGER reconcile_scan_cursors_audit
    AFTER UPDATE ON reconcile_scan_cursors
    FOR EACH ROW
    WHEN (OLD.after_holder IS DISTINCT FROM NEW.after_holder
       OR OLD.after_currency IS DISTINCT FROM NEW.after_currency
       OR OLD.lap_dirty IS DISTINCT FROM NEW.lap_dirty)
    EXECUTE FUNCTION ledger_log_reconcile_scan_cursor_change();

CREATE TRIGGER reconcile_scan_cursor_changes_no_update
    BEFORE UPDATE ON reconcile_scan_cursor_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER reconcile_scan_cursor_changes_no_delete
    BEFORE DELETE ON reconcile_scan_cursor_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

GRANT SELECT, INSERT ON public.reconcile_scan_cursor_changes TO ledger_app;
GRANT USAGE, SELECT ON public.reconcile_scan_cursor_changes_id_seq TO ledger_app;
GRANT SELECT ON public.reconcile_scan_cursor_changes TO ledger_ro;
GRANT SELECT ON public.reconcile_scan_cursor_changes_id_seq TO ledger_ro;

-- ####  §9: the same "successful narrow mutation leaves no trace" gap on
-- ####  the tables 003 already guards  ####
--
-- 003 stops currencies/classifications/journal_types/entry_templates from
-- being tampered with, but a legitimate is_active toggle, display_label
-- edit, or balance_role upgrade through those same guards still leaves no
-- record of who changed what, when -- account_policies is the only table in
-- this family with an application-level audit trail
-- (account_policy_changes), and it exists because the caller writes it in
-- the same transaction, which requires an app-level actor_id this migration
-- cannot retrofit onto the other four.
--
-- What it CAN do without touching application code: a generic AFTER UPDATE
-- trigger that records the diff and `current_user` for every change these
-- four tables' own guards let through. current_user is 'ledger_app' in every
-- deployment (that is the credential the whole guard system defends
-- against), so this does not identify a business actor -- it answers
-- "config table X changed from A to B at time T", which is what "看不出改过"
-- meant literally. Business-actor attribution needs an app-supplied column
-- and is out of this migration's file ownership.
CREATE TABLE config_table_changes (
    id         BIGSERIAL PRIMARY KEY,
    table_name TEXT NOT NULL,
    old_row    JSONB NOT NULL,
    new_row    JSONB NOT NULL,
    changed_by TEXT NOT NULL DEFAULT current_user,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_config_table_changes_table ON config_table_changes (table_name, changed_at DESC);

CREATE TRIGGER config_table_changes_no_update
    BEFORE UPDATE ON config_table_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER config_table_changes_no_delete
    BEFORE DELETE ON config_table_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

GRANT SELECT, INSERT ON public.config_table_changes TO ledger_app;
GRANT USAGE, SELECT ON public.config_table_changes_id_seq TO ledger_app;
GRANT SELECT ON public.config_table_changes TO ledger_ro;
GRANT SELECT ON public.config_table_changes_id_seq TO ledger_ro;

CREATE OR REPLACE FUNCTION ledger_log_config_table_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO config_table_changes (table_name, old_row, new_row)
    VALUES (TG_TABLE_NAME, to_jsonb(OLD), to_jsonb(NEW));
    RETURN NEW;
END;
$$;

CREATE TRIGGER currencies_audit
    AFTER UPDATE ON currencies
    FOR EACH ROW WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW))
    EXECUTE FUNCTION ledger_log_config_table_change();

CREATE TRIGGER classifications_audit
    AFTER UPDATE ON classifications
    FOR EACH ROW WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW))
    EXECUTE FUNCTION ledger_log_config_table_change();

CREATE TRIGGER journal_types_audit
    AFTER UPDATE ON journal_types
    FOR EACH ROW WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW))
    EXECUTE FUNCTION ledger_log_config_table_change();

CREATE TRIGGER entry_templates_audit
    AFTER UPDATE ON entry_templates
    FOR EACH ROW WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW))
    EXECUTE FUNCTION ledger_log_config_table_change();
