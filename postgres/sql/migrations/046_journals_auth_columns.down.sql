-- Drop the three auth columns. ledger_journals_block_arbitrary_update()
-- needs no change: it is a generic to_jsonb(OLD)/to_jsonb(NEW) comparison
-- (contracts §2, installed by migration 045) that adapts automatically to
-- whichever columns exist on the row -- there is nothing here to restore.
ALTER TABLE journals
    DROP COLUMN IF EXISTS auth_digest,
    DROP COLUMN IF EXISTS auth_signature,
    DROP COLUMN IF EXISTS auth_key_id;
