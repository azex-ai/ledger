-- Reverses 029. Every INSERT-path guard and every INSERT-side forensic
-- trigger goes away, which restores exactly the state the 2026-09-03
-- independent review measured: an appended row is neither refused nor
-- recorded. Safe as a rollback (nothing written under 029 becomes
-- unreadable; a config_table_changes row describing a creation stays valid
-- and self-describing, old_row = 'null'), and honest about what it costs.
--
-- The two owner-only doors go too. A deployment that has used
-- ledger_rewind_chain_cursor or ledger_discard_attestations_from keeps the
-- forensic rows they wrote; only the capability is removed.

DROP FUNCTION IF EXISTS ledger_discard_attestations_from(bigint, text);

DROP TRIGGER IF EXISTS entry_attestations_no_delete ON public.entry_attestations;
CREATE TRIGGER entry_attestations_no_delete
    BEFORE DELETE ON public.entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS ledger_attestations_no_delete ON public.ledger_attestations;
CREATE TRIGGER ledger_attestations_no_delete
    BEFORE DELETE ON public.ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

DROP FUNCTION IF EXISTS ledger_attestation_chain_block_delete();
DROP FUNCTION IF EXISTS ledger_attestation_discard_is_authorized();

DROP TRIGGER IF EXISTS ledger_attestations_insert_guard ON public.ledger_attestations;
DROP FUNCTION IF EXISTS ledger_attestations_insert_guard();

DROP FUNCTION IF EXISTS ledger_rewind_chain_cursor(bigint, bigint, text);
DROP TRIGGER IF EXISTS chain_cursors_audit_insert ON public.chain_cursors;
DROP TRIGGER IF EXISTS chain_cursors_audit ON public.chain_cursors;
DROP TRIGGER IF EXISTS chain_cursors_mutation_guard ON public.chain_cursors;
DROP FUNCTION IF EXISTS ledger_chain_cursors_guard();
DROP FUNCTION IF EXISTS ledger_chain_cursor_rewind_is_authorized();

DROP TRIGGER IF EXISTS reservations_insert_guard ON public.reservations;
DROP FUNCTION IF EXISTS ledger_reservations_insert_guard();

DROP TRIGGER IF EXISTS bookings_insert_guard ON public.bookings;
DROP FUNCTION IF EXISTS ledger_bookings_insert_guard();

DROP TRIGGER IF EXISTS entry_template_lines_insert_guard ON public.entry_template_lines;
DROP FUNCTION IF EXISTS ledger_entry_template_lines_insert_guard();
DROP FUNCTION IF EXISTS ledger_template_line_repair_is_authorized();

-- Every AFTER INSERT audit trigger this migration created, found the same
-- way it created them.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname AS table_name, t.tgname
        FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        JOIN pg_proc p ON p.oid = t.tgfoid
        WHERE n.nspname = 'public'
          AND NOT t.tgisinternal
          AND p.proname IN ('ledger_log_config_table_change', 'ledger_log_reconcile_scan_cursor_change')
          AND (t.tgtype & 4) <> 0   -- INSERT
    LOOP
        EXECUTE format('DROP TRIGGER %I ON public.%I', r.tgname, r.table_name);
    END LOOP;
END $$;

-- Restore 020's INSERT-unaware bodies.
CREATE OR REPLACE FUNCTION ledger_log_config_table_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (TG_TABLE_NAME, to_jsonb(OLD), to_jsonb(NEW), session_user);
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty,
        new_after_holder, new_after_currency, new_lap_dirty, changed_by
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty, session_user
    );
    RETURN NEW;
END;
$$;

-- Restore 021's dense-widening rebalance (install-roles M3 re-opens with it).
CREATE OR REPLACE FUNCTION ledger_rebalance_default_partition(
    p_first date, p_last date
) RETURNS text[]
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    created    text[] := '{}';
    v_min      timestamptz;
    v_max      timestamptz;
    v_has_rows boolean;
    v_month    date;
    v_name     text;
    max_months CONSTANT integer := 120;
BEGIN
    IF date_trunc('month', p_first)::date <> p_first OR date_trunc('month', p_last)::date <> p_last THEN
        RAISE EXCEPTION 'ledger: partition rebalance range must be month-aligned, got % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_last < p_first THEN
        RAISE EXCEPTION 'ledger: partition rebalance range ends before it starts: % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF (EXTRACT(YEAR FROM age(p_last, p_first)) * 12 + EXTRACT(MONTH FROM age(p_last, p_first))) > max_months THEN
        RAISE EXCEPTION 'ledger: partition rebalance range % .. % spans more than % months', p_first, p_last, max_months
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    ALTER TABLE journal_entries DETACH PARTITION journal_entries_default;

    SELECT min(created_at), max(created_at) INTO v_min, v_max FROM journal_entries_default;
    v_has_rows := v_min IS NOT NULL;

    IF v_has_rows THEN
        IF date_trunc('month', v_min)::date < p_first THEN
            p_first := date_trunc('month', v_min)::date;
        END IF;
        IF date_trunc('month', v_max)::date > p_last THEN
            p_last := date_trunc('month', v_max)::date;
        END IF;
    END IF;

    v_month := p_first;
    WHILE v_month <= p_last LOOP
        v_name := format('journal_entries_y%sm%s', to_char(v_month, 'YYYY'), to_char(v_month, 'MM'));
        IF to_regclass('public.' || v_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
                v_name, v_month, (v_month + INTERVAL '1 month')::date
            );
        END IF;
        created := array_append(created, v_name);
        v_month := (v_month + INTERVAL '1 month')::date;
    END LOOP;

    IF v_has_rows THEN
        INSERT INTO journal_entries SELECT * FROM journal_entries_default;
        TRUNCATE journal_entries_default;
    END IF;

    ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT;

    RETURN created;
END;
$$;
