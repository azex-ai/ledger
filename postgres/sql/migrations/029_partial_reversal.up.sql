-- Allow a journal to be reversed more than once, in fractions (see
-- core.LedgerStore.ReverseJournalFraction). Previously uq_journals_reversal_of
-- capped any given journal at exactly one reversal ever — that made partial
-- refunds impossible (a 1/3 refund followed by another 1/3 refund both point
-- reversal_of at the same original journal id).
--
-- Cumulative conservation (Σ reversed amount per entry never exceeds the
-- original entry's amount) moves from this unique index into application code
-- — see ReverseJournalFraction's SELECT ... FOR UPDATE + per-entry check.
--
-- The plain index is kept (not dropped outright) because reversal-chain
-- lookups (GetReversalChain, ListReversalsByOriginalJournalID) still filter
-- on reversal_of and would otherwise fall back to a sequential scan.
DROP INDEX IF EXISTS uq_journals_reversal_of;
CREATE INDEX IF NOT EXISTS idx_journals_reversal_of ON journals (reversal_of) WHERE reversal_of IS NOT NULL;
