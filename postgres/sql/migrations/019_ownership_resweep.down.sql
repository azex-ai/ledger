-- Ownership transfers are NOT reversed. Handing objects back to the bootstrap
-- credential is the direction this migration exists to close, and 001 records
-- why it is the dangerous one: an object's owner can CREATE OR REPLACE a guard
-- function's body and turn every append-only guarantee off without producing
-- any DDL that looks like tampering. A rollback that restores that reach is
-- worse than the state it rolls back to. Same reasoning as 007's down, which
-- likewise declines to re-widen role attributes.
--
-- ⚠️ Rollback fence for a non-superuser bootstrap: once 019 has run, the
-- objects created by 002-018 belong to ledger_owner, and the migration
-- credential holds SET but not INHERIT on that role (001, "Ownership") -- so
-- it no longer passes Postgres's ownership check for them. Running the down
-- side of 007 or 006 after this point therefore needs either a superuser
-- connection or `SET ROLE ledger_owner` around the statements. This file
-- shows the shape: SET ROLE works off the SET-only membership the credential
-- already has.
DO $$
BEGIN
    SET LOCAL ROLE ledger_owner;
    DROP FUNCTION IF EXISTS ledger_resweep_ownership();
    RESET ROLE;
END $$;
