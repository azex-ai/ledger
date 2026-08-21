ALTER TABLE entry_attestations
    DROP COLUMN IF EXISTS leaf_hash;

ALTER TABLE ledger_attestations
    DROP COLUMN IF EXISTS merkle_root;
