-- Restores the "at most one reversal per journal" constraint. This will fail
-- if any journal has been reversed more than once (e.g. by
-- ReverseJournalFraction) since the up migration ran — that data state is
-- fundamentally incompatible with the unique index, and no autoclean is
-- attempted here (no external users; caller has already decided data loss on
-- rollback is acceptable per docs/plans/2026-07-02-financial-core-hardening-design.md §0).
DROP INDEX idx_journals_reversal_of;
CREATE UNIQUE INDEX uq_journals_reversal_of ON journals (reversal_of) WHERE reversal_of IS NOT NULL;
