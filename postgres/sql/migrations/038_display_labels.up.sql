-- Holder-scoped wallet surface (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.5):
-- user-readable display labels for the transaction-view kind translation.
-- Expand-only step: NOT NULL DEFAULT '' (no-NULL convention, empty = not
-- configured — the projection falls back to `name`).
ALTER TABLE classifications ADD COLUMN IF NOT EXISTS display_label TEXT NOT NULL DEFAULT '';
ALTER TABLE journal_types   ADD COLUMN IF NOT EXISTS display_label TEXT NOT NULL DEFAULT '';
