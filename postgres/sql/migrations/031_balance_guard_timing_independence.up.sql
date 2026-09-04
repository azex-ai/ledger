-- N1 of the 2026-09-03 independent-review RECHECK
-- (`docs/audits/2026-09-03-independent-review/recheck/install-roles.md`):
-- migration 030's replacement balance guard is bypassed by one statement that
-- needs no privilege at all.
--
--     BEGIN;
--     SET CONSTRAINTS ALL IMMEDIATE;          -- the whole exploit
--     INSERT INTO public.journals (...);      -- journals-level check fires HERE,
--                                             -- on a journal with zero entries
--     INSERT INTO public.journal_entries (... 'debit', 999999);  -- one-sided
--     COMMIT;                                 -- measured: succeeds, net +999999
--
-- `SET CONSTRAINTS` applies to any DEFERRABLE constraint trigger, is available
-- to every role, and persists for the rest of the transaction. Under IMMEDIATE
-- the journals-level trigger 030 added runs at the end of the INSERT that
-- created the journal -- when it has no entries, so the aggregate is trivially
-- satisfied -- and the per-entry trigger then skips every entry because
-- `journals.xmin` is the current transaction's. Both triggers "ran"; neither
-- ever saw the entries.
--
-- The recheck confirmed three shapes: a fresh journal (above), the same with
-- only `SET CONSTRAINTS trg_check_journal_balance_on_journal IMMEDIATE`, and
-- -- the worst one -- taking over a journal that had already committed
-- balanced, by backfilling its `event_id` (a legitimate, guard-permitted
-- UPDATE) to make its xmin the current transaction's, then appending 777 of
-- unmatched debit to it. Migration 030 had anticipated that UPDATE refreshes
-- xmin and covered UPDATE on the journals trigger for exactly that reason;
-- under IMMEDIATE that patch becomes part of the attack, because the check it
-- schedules is spent at UPDATE time, before the entry arrives.
--
-- The general lesson, and the reason this is a rewrite rather than a patch:
--
--     evaluation timing is caller-controlled, so no guard may skip work on
--     the grounds that another trigger will run later.
--
-- 030's skip predicate was defended as safe in one direction ("a wrong answer
-- means the aggregate runs again, never that it is skipped for a row this
-- transaction did not write"). That reasoning holds only while the schedule
-- is fixed. It was not.
--
-- The fix is to stop having a skip at all. The per-entry constraint trigger
-- unconditionally aggregates its journal, and the journals-level trigger --
-- whose only purpose was to make the skip safe -- is dropped, along with the
-- helper that existed to keep the two in step and the EXECUTE grant that
-- helper needed. What is left is the smallest thing that can be correct: one
-- trigger, one aggregate, no state, no assumptions about when anything else
-- runs.
--
-- Cost, measured in migration 030's header on the real schema through the real
-- PostJournal path (median of 11, ms per journal):
--
--   entries/journal        2      6     20     100     500    2000
--   with the skip       3.35   3.71   6.13   20.81   96.78   363.61
--   without (this)      2.92   3.47   6.43   26.09  211.81  2268.63
--
-- Free at the sizes the presets post (2-6 entries), 5.7x at 2000. The bound is
-- O(N^2) in entries per journal and `core` sets no cap on that number, so a
-- deliberately huge journal is a real cost lever -- but it is a lever the
-- attacker pays O(N) to pull and it produces no wrong balance, which is
-- strictly better than the bypass being traded away. If a cap ever lands in
-- `core.JournalInput.Validate`, this comment is where to record the resulting
-- ceiling.
--
-- Not chosen: an owner-owned UNLOGGED memo table keyed on
-- `(pg_current_xact_id(), journal_id)`, written by a SECURITY DEFINER trigger
-- function (the recheck's first preference). It restores the O(N) curve and is
-- genuinely timing-independent, but it adds cross-transaction state to a
-- money-path trigger, and that state needs a retention story, a cleanup job
-- and its own set of "what if the cleanup does not run" questions. Not worth
-- it for a curve that only bends at journal sizes nothing legitimate produces.
--
-- Also not chosen as a fix, only noted: making the journals-level check reject
-- a journal that has no entries. It kills the two fresh-journal shapes and
-- does nothing to the third, because that journal genuinely balanced at the
-- moment the check ran.

------------------------------------------------------------------------------
-- 1. The per-entry guard stops depending on anything else running
------------------------------------------------------------------------------
--
-- Identical to 030's version minus the skip. It stays SECURITY INVOKER (it
-- reads `journal_entries`, which any caller that can trigger it can already
-- read), stays pinned to `public, pg_temp` and stays schema-qualified -- C1
-- (pg_temp relation shadowing) is closed by those two facts and this migration
-- does not disturb them.
--
-- Under `SET CONSTRAINTS ... IMMEDIATE` this now fires at the end of every
-- entry INSERT, which means an honest multi-statement journal is refused too:
-- after its first entry the journal genuinely is unbalanced. That is the correct
-- direction to fail in and nothing in this library ever issues `SET
-- CONSTRAINTS` -- the deferral is what makes multi-statement posting possible,
-- so a caller who turns it off is opting out of composing a journal across
-- statements, not opting out of the check.
CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    target_journal_id BIGINT;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM public.journal_entries
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
$$;

------------------------------------------------------------------------------
-- 2. Everything the skip needed goes away with it
------------------------------------------------------------------------------
--
-- The journals-level trigger has no coverage of its own once the per-entry
-- check is unconditional: every journal that can become unbalanced does so by
-- acquiring an entry, and every entry write runs the aggregate. Keeping it
-- would leave a second DEFERRABLE trigger whose firing time a caller can move,
-- for no gain -- so it is removed rather than demoted to defence in depth.
--
-- Known non-regression, stated so it is not mistaken for a new hole: a journal
-- row with `total_debit > 0` and no entries at all is not refused by anything
-- here. It moves no money (every balance is derived from `journal_entries`),
-- and it was equally unrefused before migration 030, whose trigger passed it
-- as trivially balanced. Detecting a `journals` row whose totals no entry
-- supports belongs to the reconcile layer, which does not currently look for
-- it; recorded as a follow-up rather than smuggled in here.
DROP TRIGGER IF EXISTS trg_check_journal_balance_on_journal ON journals;
DROP FUNCTION IF EXISTS ledger_check_journal_balance();

-- ledger_assert_journal_balanced existed so the two triggers could not drift
-- apart. With one caller left, the aggregate is inlined above and the function
-- goes -- which also withdraws the EXECUTE grant migration 030 had to give
-- ledger_app for it (a function CALLED from a SECURITY INVOKER trigger
-- function is EXECUTE-checked at call time against the invoker). DROP takes
-- the ACL with it; function_acl_test.go's whitelist shrinks back to five.
DROP FUNCTION IF EXISTS ledger_assert_journal_balanced(BIGINT);
