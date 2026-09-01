-- Widens classifications.balance_role's CHECK constraint to allow 'memo'
-- (M-4 fix, `.local/independent-review-2026-08-26.md`,
-- docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
-- fix-backend-1 batch, board #43; docs/INVARIANTS.md I-37 addendum).
--
-- core.ClassificationInput.Validate now refuses balance_role = '' on any
-- new non-system classification: '' used to mean both "this is a deliberate
-- memo/cost account, not a liability" (fee_expense) and "nobody tagged this
-- yet" -- the same value carrying two intents made the second one silently
-- invisible to SolvencyReport.Liability. Non-system memo accounts must now
-- declare 'memo' explicitly instead of leaving balance_role blank. The '' ->
-- <role> one-way upgrade guard (001_baseline / 003 / 004) already treats any
-- non-'' target the same way except for the 'available'-specific
-- has-history check, so 'memo' needs no trigger changes -- only the CHECK
-- constraint enumerating the legal values needs to widen.
--
-- 001_baseline's CHECK is unnamed in its CREATE TABLE, so Postgres assigned
-- it the standard <table>_<column>_check name.
ALTER TABLE classifications DROP CONSTRAINT IF EXISTS classifications_balance_role_check;
ALTER TABLE classifications ADD CONSTRAINT classifications_balance_role_check
    CHECK (balance_role IN ('', 'available', 'pending', 'locked', 'memo'));
