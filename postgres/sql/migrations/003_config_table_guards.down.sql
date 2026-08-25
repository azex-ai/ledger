-- Restore the pre-003 state: configuration that decides where money goes
-- becomes freely writable by ledger_app again.
--
-- Rolling this back reopens the attack described in the up migration -- an
-- application credential can repoint a deposit address at another holder, or
-- flip a template line's direction, and the resulting journals are correctly
-- signed. Roll back only to unblock, and treat the window as one in which the
-- signature guarantees mean less than they claim.
GRANT UPDATE ON public.entry_template_lines TO ledger_app;

DROP TRIGGER IF EXISTS deposit_addresses_mutation_guard ON deposit_addresses;
DROP TRIGGER IF EXISTS entry_template_lines_mutation_guard ON entry_template_lines;
DROP TRIGGER IF EXISTS entry_templates_mutation_guard ON entry_templates;
DROP TRIGGER IF EXISTS journal_types_mutation_guard ON journal_types;
DROP TRIGGER IF EXISTS currencies_mutation_guard ON currencies;

DROP FUNCTION IF EXISTS ledger_block_column_mutation();

-- Return classifications to guarding only normal_side and balance_role.
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
