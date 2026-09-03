-- Reverses 031: restores migration 030's journals-level trigger, its helper,
-- the EXECUTE grant that helper needs, and the xmin skip in the per-entry
-- guard.
--
-- Going down re-opens N1 -- a caller can bypass the balance guard with one
-- `SET CONSTRAINTS ALL IMMEDIATE`. That is what a down script is for, and it
-- is written here so nobody reads this file as an alternative implementation.

CREATE FUNCTION ledger_assert_journal_balanced(p_journal_id BIGINT) RETURNS void
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM public.journal_entries
        WHERE journal_id = p_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END
        ) <> 0
    ) THEN
        RAISE EXCEPTION 'journal % has unbalanced entries by currency', p_journal_id
            USING
                ERRCODE = '23514',
                CONSTRAINT = 'chk_journal_currency_balance';
    END IF;
END;
$$;

CREATE FUNCTION ledger_check_journal_balance() RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    PERFORM public.ledger_assert_journal_balanced(NEW.id);
    RETURN NULL;
END;
$$;

ALTER FUNCTION ledger_assert_journal_balanced(BIGINT) OWNER TO ledger_owner;
ALTER FUNCTION ledger_check_journal_balance()         OWNER TO ledger_owner;

REVOKE ALL ON FUNCTION ledger_assert_journal_balanced(BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION ledger_check_journal_balance()         FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_assert_journal_balanced(BIGINT) TO ledger_app;

CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    target_journal_id BIGINT;
    written_here      BOOLEAN;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT j.xmin = pg_catalog.pg_current_xact_id()::xid
      INTO written_here
      FROM public.journals j
     WHERE j.id = target_journal_id;

    IF COALESCE(written_here, FALSE) THEN
        RETURN NULL;
    END IF;

    PERFORM public.ledger_assert_journal_balanced(target_journal_id);
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_check_journal_balance_on_journal
    AFTER INSERT OR UPDATE ON journals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_check_journal_balance();
