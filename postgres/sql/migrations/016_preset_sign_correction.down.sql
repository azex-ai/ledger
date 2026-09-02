-- Reinstate the pre-016 template directions and equity polarity.
--
-- This is a genuine down migration -- it restores the exact rows 016 replaced
-- -- but running it puts five templates back into the state the 2026-09-02
-- audit measured as producing wrong money: capital injection destroying the
-- solvency margin, settled merchants being debited their own takings, and fee
-- revenue counting down. Use it to roll a release back, not to "revert a
-- style choice".
--
-- Journal entries posted while 016 was in effect are NOT rewritten (the
-- ledger is append-only), so a rollback leaves history written under two
-- different template directions. Reconcile before rolling forward again.

ALTER TABLE entry_template_lines DISABLE TRIGGER entry_template_lines_mutation_guard;
ALTER TABLE classifications      DISABLE TRIGGER classifications_mutation_guard;

DELETE FROM balance_checkpoints
WHERE classification_id IN (SELECT id FROM classifications WHERE code = 'equity');

DELETE FROM balance_snapshots
WHERE classification_id IN (SELECT id FROM classifications WHERE code = 'equity');

UPDATE classifications
SET normal_side = 'credit'
WHERE code = 'equity'
  AND normal_side = 'debit';

CREATE FUNCTION ledger_replace_template_lines_016_down(
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
            v_template_id, c.id, p_lines[i][2], p_lines[i][3], p_lines[i][4], p_lines[i][5]::int
        FROM classifications c
        WHERE c.code = p_lines[i][1];
    END LOOP;

    SELECT COALESCE(jsonb_agg(to_jsonb(l) ORDER BY l.sort_order), '[]'::jsonb)
      INTO v_new
      FROM entry_template_lines l
     WHERE l.template_id = v_template_id;

    INSERT INTO config_table_changes (table_name, old_row, new_row)
    VALUES (
        'entry_template_lines',
        jsonb_build_object('template_code', p_template_code, 'lines', v_old),
        jsonb_build_object('template_code', p_template_code, 'lines', v_new,
                           'migration', '016_preset_sign_correction.down')
    );
END;
$$;

SELECT ledger_replace_template_lines_016_down('capital_injection', ARRAY[
    ARRAY['custodial', 'debit',  'system', 'amount', '1'],
    ARRAY['equity',    'credit', 'system', 'amount', '2']
]);

SELECT ledger_replace_template_lines_016_down('capital_withdraw', ARRAY[
    ARRAY['equity',    'debit',  'system', 'amount', '1'],
    ARRAY['custodial', 'credit', 'system', 'amount', '2']
]);

SELECT ledger_replace_template_lines_016_down('checkout_settlement_gross', ARRAY[
    ARRAY['custodial',   'debit',  'system', 'gross_amount', '1'],
    ARRAY['main_wallet', 'credit', 'user',   'gross_amount', '2']
]);

SELECT ledger_replace_template_lines_016_down('checkout_settlement_net', ARRAY[
    ARRAY['custodial',   'debit',  'system', 'gross_amount', '1'],
    ARRAY['main_wallet', 'credit', 'user',   'net_amount',   '2'],
    ARRAY['fees',        'credit', 'system', 'fee_amount',   '3']
]);

SELECT ledger_replace_template_lines_016_down('fee_charge', ARRAY[
    ARRAY['main_wallet', 'credit', 'user',   'amount', '1'],
    ARRAY['fees',        'debit',  'system', 'amount', '2']
]);

DROP FUNCTION ledger_replace_template_lines_016_down(text, text[][]);

ALTER TABLE entry_template_lines ENABLE TRIGGER entry_template_lines_mutation_guard;
ALTER TABLE classifications      ENABLE TRIGGER classifications_mutation_guard;
