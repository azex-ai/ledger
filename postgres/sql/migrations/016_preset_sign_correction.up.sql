-- Correct the direction of five shipped entry templates, and flip the polarity
-- of the `equity` classification that one of them needs.
--
-- ####  What was wrong  ####
--
-- This ledger does not have a global "assets are debit-normal" convention.
-- Each classification declares its own normal_side, a leg increases its
-- account when entry_type == normal_side (ledger_signed_amount, migration
-- 009), and a journal must satisfy sum(DR) == sum(CR). The consequence a
-- template author has to hold in mind is narrow and unforgiving: two accounts
-- that BOTH INCREASE in the same journal must carry OPPOSITE normal_sides.
--
-- Five shipped templates were written to standard accounting's "debit an
-- asset to increase it" instead, against a `custodial` account this ledger
-- declares credit-normal. The 2026-09-02 audit measured each one:
--
--   capital_injection          injecting 1000 of platform capital moved the
--                              custody figure to -500 and pinned
--                              SolvencyCheck at solvent=false permanently --
--                              the only action that exists to improve
--                              solvency was the one that destroyed it.
--   capital_withdraw           the symmetric inverse: taking capital OUT
--                              increased custody.
--   checkout_settlement_gross  a settled merchant was DEBITED their own
--                              takings, and custody fell by the gross.
--   checkout_settlement_net    same, plus a fresh -fee of phantom insolvency
--                              on every settlement, accumulating forever.
--   fee_charge                 debited credit-normal `fees`, so revenue
--                              counted DOWN; two 30-unit fees collected
--                              through the two shipped fee paths summed to
--                              zero.
--
-- The correct shapes now live in presets/capital.go, presets/settlement.go
-- and presets/fee.go, each with the arithmetic written out. This migration
-- brings already-installed databases to the same place.
--
-- ####  Why a migration and not just the Go change  ####
--
-- InstallTemplatePresets never updates a template that already exists -- it
-- validates and errors (presets/templates.go, validateExistingTemplatePreset).
-- On top of that, entry_template_lines carries a BEFORE UPDATE guard that
-- permits no column change at all and has had UPDATE revoked from ledger_app
-- (migration 003), because a template line is what decides which account each
-- leg hits and in which direction -- exactly the thing an attacker with the
-- application credential would want to move. That guard is doing its job;
-- the only sanctioned way past it is a migration running as the owner.
--
-- So: disable the guards for the length of this transaction, rewrite the
-- rows, record what changed in config_table_changes, re-enable. The
-- alternative -- shipping capital_injection_v2 and friends -- was rejected:
-- this library has no external consumers yet, and a permanent naming scar on
-- five templates is a worse legacy than one migration.
--
-- Line COUNTS change (fee_charge 2 -> 4, checkout_settlement_net 3 -> 4), so
-- the lines are deleted and reinserted rather than updated. Everything is
-- guarded by EXISTS on the template code, so a deployment that never
-- installed these bundles is untouched.
--
-- ####  equity  ####
--
-- capital_injection has to raise custodial AND equity in one journal.
-- custodial is credit-normal, so equity must be debit-normal for that journal
-- to exist at all. classifications.normal_side is immutable by trigger, and
-- for good reason -- it fixes the sign of every historical entry for that
-- classification. Flipping it is therefore only defensible together with
-- invalidating the caches derived from that sign, which this migration also
-- does: balance_checkpoints and balance_snapshots rows for equity are
-- deleted, and both are pure caches (balance = checkpoint.balance +
-- SUM(entries > last_entry_id), so no checkpoint row means a full recompute
-- from journal_entries, which is the correct answer).
--
-- What is NOT repaired, and cannot be: journal_entries already posted by the
-- old templates. The ledger is append-only; historical mis-postings are
-- corrected by posting a reversal, not by rewriting rows. An operator with
-- capital, checkout-settlement or direct-fee history must reverse and repost
-- it. This is recorded in the release's breaking-change list.

------------------------------------------------------------------------------
-- 1. Suspend the config guards for this transaction.
------------------------------------------------------------------------------

ALTER TABLE entry_template_lines DISABLE TRIGGER entry_template_lines_mutation_guard;
ALTER TABLE classifications      DISABLE TRIGGER classifications_mutation_guard;

------------------------------------------------------------------------------
-- 2. equity: credit-normal -> debit-normal, with its derived caches dropped.
--
-- The AFTER UPDATE classifications_audit trigger (migration 006) records this
-- row change in config_table_changes on its own; no manual audit row needed.
------------------------------------------------------------------------------

DELETE FROM balance_checkpoints
WHERE classification_id IN (SELECT id FROM classifications WHERE code = 'equity');

DELETE FROM balance_snapshots
WHERE classification_id IN (SELECT id FROM classifications WHERE code = 'equity');

UPDATE classifications
SET normal_side = 'debit'
WHERE code = 'equity'
  AND normal_side = 'credit';

------------------------------------------------------------------------------
-- 3. Rewrite the five templates' lines.
--
-- ledger_replace_template_lines_016 keeps the five rewrites readable: each
-- call names a template code and the lines it should end up with, as
-- (classification_code, entry_type, holder_role, amount_key, sort_order)
-- tuples in sort order. It is dropped at the end of the migration -- it is a
-- local helper, not a permanent function anybody could call later.
------------------------------------------------------------------------------

CREATE FUNCTION ledger_replace_template_lines_016(
    p_template_code text,
    p_lines         text[][]
) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    v_template_id bigint;
    v_old         jsonb;
    v_new         jsonb;
    i             int;
BEGIN
    SELECT id INTO v_template_id FROM entry_templates WHERE code = p_template_code;
    IF v_template_id IS NULL THEN
        -- This deployment never installed the bundle. Nothing to correct.
        RETURN;
    END IF;

    SELECT COALESCE(jsonb_agg(to_jsonb(l) ORDER BY l.sort_order), '[]'::jsonb)
      INTO v_old
      FROM entry_template_lines l
     WHERE l.template_id = v_template_id;

    DELETE FROM entry_template_lines WHERE template_id = v_template_id;

    FOR i IN 1 .. array_length(p_lines, 1) LOOP
        INSERT INTO entry_template_lines
            (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
        SELECT
            v_template_id,
            c.id,
            p_lines[i][2],
            p_lines[i][3],
            p_lines[i][4],
            p_lines[i][5]::int
        FROM classifications c
        WHERE c.code = p_lines[i][1];

        IF NOT FOUND THEN
            RAISE EXCEPTION
                'ledger: migration 016: template % needs classification %, which does not exist; install the matching preset bundle before upgrading',
                p_template_code, p_lines[i][1]
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    END LOOP;

    SELECT COALESCE(jsonb_agg(to_jsonb(l) ORDER BY l.sort_order), '[]'::jsonb)
      INTO v_new
      FROM entry_template_lines l
     WHERE l.template_id = v_template_id;

    -- entry_template_lines has no AFTER-trigger audit of its own (migration
    -- 006 wired the four tables whose guards permit narrow updates; this one
    -- permits none, so nothing could ever have fired). The audit row is
    -- written explicitly here so the one sanctioned edit in this table's
    -- history is visible to the same reader who looks at every other config
    -- change.
    INSERT INTO config_table_changes (table_name, old_row, new_row)
    VALUES (
        'entry_template_lines',
        jsonb_build_object('template_code', p_template_code, 'lines', v_old),
        jsonb_build_object('template_code', p_template_code, 'lines', v_new,
                           'migration', '016_preset_sign_correction')
    );
END;
$$;

-- capital_injection: CR custodial (+amount), DR equity (+amount).
SELECT ledger_replace_template_lines_016('capital_injection', ARRAY[
    ARRAY['custodial', 'credit', 'system', 'amount', '1'],
    ARRAY['equity',    'debit',  'system', 'amount', '2']
]);

-- capital_withdraw: DR custodial (-amount), CR equity (-amount).
SELECT ledger_replace_template_lines_016('capital_withdraw', ARRAY[
    ARRAY['custodial', 'debit',  'system', 'amount', '1'],
    ARRAY['equity',    'credit', 'system', 'amount', '2']
]);

-- checkout_settlement_gross: the merchant receives the whole gross.
SELECT ledger_replace_template_lines_016('checkout_settlement_gross', ARRAY[
    ARRAY['main_wallet', 'debit',  'user',   'gross_amount', '1'],
    ARRAY['custodial',   'credit', 'system', 'gross_amount', '2']
]);

-- checkout_settlement_net: gross splits into the merchant's claim (custody
-- backs it) and the platform's fee (revenue). No gross leg -- see
-- presets/settlement.go for why one cannot exist.
SELECT ledger_replace_template_lines_016('checkout_settlement_net', ARRAY[
    ARRAY['main_wallet', 'debit',  'user',   'net_amount', '1'],
    ARRAY['fee_expense', 'debit',  'user',   'fee_amount', '2'],
    ARRAY['custodial',   'credit', 'system', 'net_amount', '3'],
    ARRAY['fees',        'credit', 'system', 'fee_amount', '4']
]);

-- fee_charge: the withdraw_fee shape, with fees standing in for fee_revenue.
SELECT ledger_replace_template_lines_016('fee_charge', ARRAY[
    ARRAY['fee_expense', 'debit',  'user',   'amount', '1'],
    ARRAY['custodial',   'debit',  'system', 'amount', '2'],
    ARRAY['main_wallet', 'credit', 'user',   'amount', '3'],
    ARRAY['fees',        'credit', 'system', 'amount', '4']
]);

DROP FUNCTION ledger_replace_template_lines_016(text, text[][]);

------------------------------------------------------------------------------
-- 4. Restore the guards.
------------------------------------------------------------------------------

ALTER TABLE entry_template_lines ENABLE TRIGGER entry_template_lines_mutation_guard;
ALTER TABLE classifications      ENABLE TRIGGER classifications_mutation_guard;
