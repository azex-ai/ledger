-- Add soft-delete support to currencies, matching classifications / journal_types
-- / templates. A currency is never hard-deleted (it is referenced by historical
-- journal entries); deactivation hides it from active listings while preserving
-- referential integrity. Defaulting to true keeps every existing row active.
ALTER TABLE currencies
    ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true;
