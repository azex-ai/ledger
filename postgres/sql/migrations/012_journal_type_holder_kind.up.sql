-- Adds journal_types.holder_kind (M-7 fix,
-- `.local/independent-review-2026-08-26.md`,
-- docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
-- fix-m7-kind batch, board #49; docs/INVARIANTS.md I-44).
--
-- The holder-facing transaction view's `kind` field (postgres/sql/queries/
-- holder.sql's ListHolderTransactionRows) went through two shapes before
-- this one: journal_types.code (e.g. "deposit_confirm" -- an internal
-- accounting-engine identifier that narrates how the ledger produced the
-- balance, a ~/.claude/rules/user-facing-surfaces.md violation), then
-- journal_types.uid (compliant, but opaque and per-deployment-random, so
-- @azex/ledger-react's kindLabels prop -- keyed by a stable literal a host
-- app hardcodes -- could never match it). This column is the third shape: a
-- small, deployment-stable product vocabulary (core.HolderTxKind) a journal
-- type declares itself under once, that a host app CAN hardcode a
-- kindLabels map against.
--
-- Nullability: NOT NULL DEFAULT '' (expand-safe, financial.md's "no NULL"
-- convention) -- '' means "untagged", the same way BalanceRoleNone did
-- before the M-4 fix required non-system classifications to declare a
-- balance_role explicitly. This column is deliberately NOT made required at
-- creation the way M-4 made balance_role required: CreateJournalType is a
-- generic, everywhere-called constructor (test scaffolding across postgres/
-- and service/, the examples/ package, the POST /journal-types HTTP
-- handler), and unlike balance_role -- which feeds SolvencyReport.Liability,
-- a financial-correctness computation that goes silently wrong when it is
-- missing -- an untagged holder_kind has no financial consequence. The read
-- path (ListHolderTransactionRows below) never emits '' on the wire; it
-- reads an untagged row as 'other', a legitimate, disclosed, generic
-- bucket, not a silent miscalculation. See core.HolderTxKindNone's doc
-- comment for the full reasoning.
ALTER TABLE journal_types ADD COLUMN holder_kind TEXT NOT NULL DEFAULT ''
    CHECK (holder_kind IN ('', 'deposit', 'withdrawal', 'transfer', 'fee', 'adjustment', 'other'));

-- migration 003's journal_types_mutation_guard whitelists exactly the
-- columns a legitimate UPDATE may touch (comment there: "The whitelist is
-- compared generically ... so a column added by a future migration is
-- protected without anyone remembering to extend anything" -- protected by
-- DEFAULT REFUSAL, not by silence: an UPDATE that only sets holder_kind
-- would otherwise be rejected with "may only change display_label,
-- is_active" the same way an UPDATE touching normal_side on classifications
-- is). Re-creating the trigger with holder_kind added is that "extending"
-- step -- 003's guard function itself is untouched, only this table's
-- trigger's argument list.
DROP TRIGGER journal_types_mutation_guard ON journal_types;
CREATE TRIGGER journal_types_mutation_guard
    BEFORE UPDATE ON journal_types
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation('display_label', 'is_active', 'holder_kind');

-- Backfill every journal type this package's own presets (presets/*.go)
-- install, so a fresh install of any bundle never has to fall back to the
-- 'other' default for its own preset rows, and an upgrade of an existing
-- deployment that already installed these presets is retagged in place
-- without a code change. Codes not listed here (a consumer's own custom
-- journal types) are left at '' -- this migration cannot know what they
-- mean; JournalTypeStore.SetHolderKind lets a deployer tag them explicitly.
UPDATE journal_types SET holder_kind = 'deposit' WHERE code IN (
    'deposit_pending', 'deposit_confirm', 'deposit_confirm_pending',
    'deposit_release_pending', 'checkout_settlement'
);
UPDATE journal_types SET holder_kind = 'withdrawal' WHERE code IN (
    'lock_funds', 'unlock_funds', 'withdraw_confirm'
);
UPDATE journal_types SET holder_kind = 'fee' WHERE code IN (
    'withdraw_fee', 'fee'
);
UPDATE journal_types SET holder_kind = 'transfer' WHERE code IN (
    'transfer'
);
UPDATE journal_types SET holder_kind = 'adjustment' WHERE code IN (
    'deposit_record_overage', 'deposit_resolve_overage',
    'deposit_release_overage', 'dev_credit'
);
UPDATE journal_types SET holder_kind = 'other' WHERE code IN (
    'capital_injection', 'capital_withdraw', 'fx_sell', 'fx_buy'
);
