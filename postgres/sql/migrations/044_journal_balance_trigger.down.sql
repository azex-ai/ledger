-- 044_journal_balance_trigger.down.sql
--
-- Reverts to the 018 state: no DB-layer per-journal balance enforcement.
-- The application-layer VerifyJournalBalanced check (queries/journals.sql)
-- is untouched by this migration and keeps running either way.

DROP TRIGGER IF EXISTS trg_check_journal_currency_balance ON journal_entries;
DROP FUNCTION IF EXISTS check_journal_currency_balance();
