-- Make the tables that decide where money goes as immutable as the money
-- itself.
--
-- The audit found that per-journal authorization signatures authenticate what
-- the application READ, not what happened. IngestDeposit resolves a holder
-- from deposit_addresses and hands it to the Attestor; EntryTemplate.Render
-- reads entry_template_lines fresh on every call and the signature covers the
-- rendered result. Both tables were writable by ledger_app -- the exact
-- credential the threat model assumes is compromised -- and neither carried a
-- mutation guard.
--
-- The attack does not forge anything. It makes the application sign a correct
-- journal about the wrong facts:
--
--     UPDATE deposit_addresses SET account_holder = <attacker>
--      WHERE address = '<victim>';
--
-- The victim's next real deposit is credited to the attacker, and the journal
-- that does it is signed, chain-attested, verdict-authorized, and reports
-- VERIFIED. Solvency balances, because the money genuinely arrived; it is
-- attributed to the wrong person. Every layer says fine.
--
-- Both statements were run against a real database as ledger_app before this
-- migration was written. Both succeeded.
--
-- Coverage at the time: 14 tables carried a mutation guard, and 22 tables
-- ledger_app could UPDATE carried none.
--
-- ####  Why a column whitelist and not a blanket refusal  ####
--
-- Every table below has at most a handful of legitimate mutations, and two
-- have none at all. Taken from the queries rather than from memory:
--
--     currencies             is_active
--     classifications        display_label, balance_role, lifecycle, is_active
--     journal_types          display_label, is_active
--     entry_templates        is_active
--     entry_template_lines   (none)
--     deposit_addresses      (none)
--
-- What that leaves immutable is the point: currencies.exponent (the precision
-- every amount is validated against), classifications.normal_side and .code
-- (the sign of every historical rollup, and the key presets resolve by), every
-- column of a template line (which account each leg hits and in which
-- direction), and the holder a deposit address belongs to.
--
-- The whitelist is compared generically -- to_jsonb(OLD) minus the allowed
-- keys against to_jsonb(NEW) minus the same -- so a column added by a future
-- migration is protected without anyone remembering to extend anything. This
-- is the shape migration 045 arrived at for journals after the hardcoded
-- protected-column list was silently broken twice.
--
-- It also handles a case a blanket refusal would break. RegisterDepositAddress
-- is an upsert whose conflict branch sets account_holder to its own value, so
-- RETURNING yields the existing row; nothing changes, so the generic
-- comparison passes it while still refusing any real edit. An unconditional
-- BEFORE UPDATE trigger would have made address registration non-idempotent.

CREATE OR REPLACE FUNCTION ledger_block_column_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- The columns this table's trigger declares mutable, passed at CREATE
    -- TRIGGER so the whitelist is readable where the table is named rather
    -- than buried in a shared function body.
    --
    -- COALESCE is load-bearing: TG_ARGV is NULL, not an empty array, when the
    -- trigger is created with no arguments -- which is exactly the case for
    -- the two tables that allow no mutation at all. Without it, `to_jsonb(OLD)
    -- - NULL` is NULL, `NULL IS DISTINCT FROM NULL` is false, and the guard
    -- installs, fires, and permits everything. Caught by running the attack
    -- against the guard: four tables refused it and deposit_addresses did not.
    mutable CONSTANT text[] := COALESCE(TG_ARGV, '{}'::text[]);
BEGIN
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on % may only change %, and this statement changed something else',
            TG_TABLE_NAME,
            CASE WHEN cardinality(mutable) = 0 THEN 'nothing' ELSE array_to_string(mutable, ', ') END
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER currencies_mutation_guard
    BEFORE UPDATE ON currencies
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation('is_active');

CREATE TRIGGER journal_types_mutation_guard
    BEFORE UPDATE ON journal_types
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation('display_label', 'is_active');

CREATE TRIGGER entry_templates_mutation_guard
    BEFORE UPDATE ON entry_templates
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation('is_active');

CREATE TRIGGER entry_template_lines_mutation_guard
    BEFORE UPDATE ON entry_template_lines
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation();

CREATE TRIGGER deposit_addresses_mutation_guard
    BEFORE UPDATE ON deposit_addresses
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation();

-- classifications already refuses two specific changes: normal_side is
-- immutable because it decides the sign of every historical rollup, and
-- balance_role may only go from '' to a role because switching between roles
-- re-buckets a holder's available/pending/locked view with no accounting event
-- behind it. Those rules stay; the whitelist is added underneath so that code,
-- name, is_system, uid and anything added later are refused by default
-- instead of by omission.
CREATE OR REPLACE FUNCTION ledger_classifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['display_label', 'balance_role', 'lifecycle', 'is_active'];
BEGIN
    IF NEW.normal_side IS DISTINCT FROM OLD.normal_side THEN
        RAISE EXCEPTION 'ledger: classifications.normal_side is immutable; it determines the sign of every historical rollup for this classification'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_role IS DISTINCT FROM OLD.balance_role AND OLD.balance_role <> '' THEN
        RAISE EXCEPTION 'ledger: classifications.balance_role is already set to %; only the '''' -> <role> upgrade is allowed', OLD.balance_role
            USING ERRCODE = 'check_violation';
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on classifications may only change %, and this statement changed something else -- code and normal_side in particular are what presets resolve by and what fixes the sign of historical entries',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

-- entry_template_lines has no legitimate UPDATE at all -- no upsert, no
-- deactivation, nothing. The trigger already refuses every change; taking the
-- privilege away too means the ACL and the guard say the same thing, which is
-- the consistency 001_baseline's section 14 argues for: dropping a trigger
-- needs ownership, while UPDATE needs only a grant, so they defend against
-- different bypasses.
--
-- The other five keep their UPDATE grant because each still has a legitimate
-- narrow mutation, or -- for deposit_addresses -- a no-op upsert.
REVOKE UPDATE ON public.entry_template_lines FROM ledger_app;
