-- Intentionally a no-op: reversing would mean merging monthly partitions
-- back into the default partition — a data move with no operational value
-- and real risk. Monthly partitions are forward-compatible with the pre-037
-- schema (the parent table and default partition are unchanged), so a code
-- rollback works without touching the data layout.
SELECT 1;
