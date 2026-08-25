-- Refuse the '' -> 'available' upgrade once a classification has history.
--
-- balance_role buckets a classification into the holder-facing breakdown, and
-- 'available' is the only bucket Reserve spends from: reserveWithQueries takes
-- availableBase from roleSums[available], and classifications with role ''
-- are skipped entirely when the sums are computed.
--
-- The guard installed with the schema allows '' -> <role> as a one-way
-- upgrade, described as the move a deployment makes "once, when it starts
-- opting a classification into the breakdown". Nothing restricted it to that
-- moment, and one statement is enough:
--
--     UPDATE classifications SET balance_role = 'available'
--      WHERE code = 'fee_expense';
--
-- fee_expense ships with the withdrawal preset. It is user-side and
-- debit-normal, and withdraw_fee debits it on every withdrawal, so each
-- holder's fee_expense balance is the running total of fees they have paid.
-- That statement turns every holder's fee history into spendable balance at
-- once. A holder who has paid 1,200 in fees can then reserve 1,200 and
-- withdraw it through an ordinary, correctly signed withdraw_confirm.
--
-- None of the layers above notice, because nothing about the accounting is
-- wrong. The entries are real, the signatures are valid, debits equal credits,
-- and the global accounting identity is untouched. What changed is which
-- bucket the money is counted in, and no invariant covers that.
-- RequireVerifiedBalance does not help either: it verifies the journals behind
-- whatever is currently bucketed as available, and those journals are genuine.
--
-- The exposure is not limited to fee_expense. Any user-side classification
-- with role '' can be promoted the same way, and classification-driven design
-- is what this library is for -- escrow, unvested, accrued balances are
-- exactly the kind of thing consumers define.
--
-- ####  What this migration allows and what it refuses  ####
--
-- The upgrade stays free when it cannot make anyone richer:
--
--   '' -> 'pending'  and  '' -> 'locked'   always allowed. Neither bucket is
--                                          spendable; Reserve reads only
--                                          'available'.
--   '' -> 'available' on a classification with no journal entries yet
--                                          allowed. This is the install-time
--                                          case the original rule described,
--                                          and with no entries there is no
--                                          balance to promote.
--   '' -> 'available' on a classification that already has entries
--                                          REFUSED. This is the attack, and it
--                                          is also the only shape in which a
--                                          legitimate late adoption moves real
--                                          money between buckets -- which
--                                          makes it a decision that should
--                                          cost more than one UPDATE.
--
-- A deployment that genuinely wants to promote a classification with history
-- still can: as ledger_owner, drop the trigger, make the change, and put it
-- back. That is deliberately more effort than the application credential has.
-- The point is not that it is impossible; it is that it is not one statement
-- available to a leaked application credential.
--
-- Preset installation is unaffected. ensureClassificationPreset creates new
-- classifications with their balance_role already set on the INSERT; the
-- UPDATE path only exists for rows that predate the role, which is where the
-- money question lives.
CREATE OR REPLACE FUNCTION ledger_classifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['display_label', 'balance_role', 'lifecycle', 'is_active'];
    has_history boolean;
BEGIN
    IF NEW.normal_side IS DISTINCT FROM OLD.normal_side THEN
        RAISE EXCEPTION 'ledger: classifications.normal_side is immutable; it determines the sign of every historical rollup for this classification'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_role IS DISTINCT FROM OLD.balance_role THEN
        IF OLD.balance_role <> '' THEN
            RAISE EXCEPTION 'ledger: classifications.balance_role is already set to %; only the '''' -> <role> upgrade is allowed', OLD.balance_role
                USING ERRCODE = 'check_violation';
        END IF;

        -- Only 'available' is spendable, so only 'available' needs the
        -- history check. Reading journal_entries from a trigger is fine here:
        -- this table is configuration, written a handful of times per
        -- deployment, and the index on (classification_id) already exists for
        -- the balance read path.
        IF NEW.balance_role = 'available' THEN
            SELECT EXISTS (SELECT 1 FROM journal_entries WHERE classification_id = NEW.id) INTO has_history;
            IF has_history THEN
                RAISE EXCEPTION 'ledger: classification % already has journal entries, so promoting it to balance_role=''available'' would turn existing balances into spendable funds; do this as ledger_owner with the guard dropped if it is genuinely intended', NEW.code
                    USING ERRCODE = 'check_violation';
            END IF;
        END IF;
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on classifications may only change %, and this statement changed something else -- code and normal_side in particular are what presets resolve by and what fixes the sign of historical entries',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;
