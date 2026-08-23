-- 054_attested_auth_verdict.down.sql
ALTER TABLE ledger_attestations
    DROP COLUMN IF EXISTS auth_verdict_digest;

ALTER TABLE entry_attestations
    DROP COLUMN IF EXISTS auth_verdict;
