-- journal_entries (migration 004) is PARTITION BY RANGE (created_at) but has
-- no primary key — only a BIGSERIAL id column and two plain indexes. A
-- partitioned table's primary key must include the partition key, so this is
-- (id, created_at) rather than id alone. This backs logical replication
-- (REPLICA IDENTITY needs a PK or explicit replica identity) and gives a
-- uniqueness backstop beyond "we trust the sequence".
--
-- Postgres has no `ADD CONSTRAINT IF NOT EXISTS`, so idempotency is done via
-- an explicit pg_constraint check.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'journal_entries'::regclass
          AND contype = 'p'
    ) THEN
        ALTER TABLE journal_entries ADD PRIMARY KEY (id, created_at);
    END IF;
END $$;
