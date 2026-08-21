-- Rollback note (deployment.md): only safe immediately after deploy, before
-- any real RebuildCheckpoint call has written a row -- dropping this table
-- after it holds forensic evidence destroys that evidence permanently, the
-- same "breaking-rollback-fence" concern as deleting attestation rows
-- (docs/plans/2026-08-21-tamper-evident-ledger-design.md §12).
DROP TABLE IF EXISTS checkpoint_rebuilds;
