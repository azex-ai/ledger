-- Reverses 030: unpins the ten functions it pinned, restores the pg_temp
-- dedup form of check_journal_currency_balance() exactly as 001 shipped it,
-- drops the journals-level constraint trigger and its two helpers, and hands
-- TEMPORARY back to PUBLIC.
--
-- Going down re-opens C1 and C2. That is what a down script is for -- getting
-- back to the previous release's behaviour, defects included -- but it is
-- worth writing down here so nobody reads this file as an alternative
-- implementation.

DROP TRIGGER IF EXISTS trg_check_journal_balance_on_journal ON journals;

CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER AS $$
DECLARE
    target_journal_id BIGINT;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    CREATE TEMP TABLE IF NOT EXISTS ledger_balance_checked (
        journal_id BIGINT PRIMARY KEY
    ) ON COMMIT DELETE ROWS;

    INSERT INTO ledger_balance_checked (journal_id)
    VALUES (target_journal_id)
    ON CONFLICT DO NOTHING;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM journal_entries
        WHERE journal_id = target_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END
        ) <> 0
    ) THEN
        RAISE EXCEPTION 'journal % has unbalanced entries by currency', target_journal_id
            USING
                ERRCODE = '23514',
                CONSTRAINT = 'chk_journal_currency_balance';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- CREATE OR REPLACE keeps a function's existing proconfig, so the pin has to
-- be dropped explicitly even though the body above no longer needs it.
ALTER FUNCTION check_journal_currency_balance() RESET search_path;

DROP FUNCTION IF EXISTS ledger_check_journal_balance();
DROP FUNCTION IF EXISTS ledger_assert_journal_balanced(BIGINT);

ALTER FUNCTION ledger_block_mutation()                  RESET search_path;
ALTER FUNCTION ledger_block_column_mutation()           RESET search_path;
ALTER FUNCTION ledger_journals_block_arbitrary_update() RESET search_path;
ALTER FUNCTION ledger_classifications_guard()           RESET search_path;
ALTER FUNCTION ledger_reservations_guard()              RESET search_path;
ALTER FUNCTION ledger_account_policies_guard()          RESET search_path;
ALTER FUNCTION ledger_bookings_guard()                  RESET search_path;
ALTER FUNCTION ledger_events_guard()                    RESET search_path;
ALTER FUNCTION ledger_reject_unknown_normal_side(text)  RESET search_path;
ALTER FUNCTION ledger_resweep_ownership()               RESET search_path;

-- Same role manoeuvre as the up script: only the database owner can grant a
-- database privilege, and this connection is ledger_owner until COMMIT.
-- Failing to restore TEMPORARY is not worth aborting a rollback over -- the
-- up direction is the one where a missing revoke is a security fact -- so
-- this half is best-effort and says so out loud.
SET LOCAL ROLE NONE;

DO $$
BEGIN
    EXECUTE pg_catalog.format(
        'GRANT TEMPORARY ON DATABASE %I TO PUBLIC',
        pg_catalog.current_database());
EXCEPTION WHEN insufficient_privilege THEN
    RAISE WARNING
        'ledger: could not restore TEMPORARY to PUBLIC as % -- if the pre-030 behaviour is wanted, run this as the database owner: %',
        pg_catalog.current_user,
        pg_catalog.format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', pg_catalog.current_database());
END $$;
