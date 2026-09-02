-- balance_checkpoints / rollup_queue / balance_snapshots: give the dimension
-- columns the same referential integrity system_rollups has always had
-- (2026-09-02 deep audit, structure-and-contract.md H-m8).
--
-- One schema, two policies for the same kind of column: system_rollups
-- declares `currency_id BIGINT NOT NULL REFERENCES currencies(id)` and
-- `classification_id ... REFERENCES classifications(id)`
-- (001_baseline.up.sql §5), while its three sibling tables in the same
-- section carry bare BIGINTs. 001's comments explain the one deliberate
-- exception in this area -- account_policies.currency_id = 0 as a wildcard --
-- and say nothing about these three, so the difference reads as an oversight
-- rather than a decision, and nothing prevented a row pointing at a
-- classification that does not exist.
--
-- Why it matters beyond tidiness: these three tables are the balance
-- materialization path. AggregateCheckpointsByClassification
-- (checkpoints.sql) sums balance_checkpoints INTO system_rollups, so an
-- orphan checkpoint row is summed into a table whose own FKs guarantee it
-- cannot contain that dimension -- the constraint was enforced at the
-- destination and not at the source. Today only the orphan_* reconcile
-- checks can find such a row, after the fact.
--
-- These are cache/queue tables, so the pre-flight below DELETEs orphans
-- rather than refusing to migrate. That is safe in a way it would not be for
-- journal_entries: a checkpoint row is derived (balance = checkpoint +
-- entries above its watermark, I-5), so deleting one costs a full
-- recomputation on the next read and loses no truth. A snapshot row for a
-- dimension that no longer exists cannot be joined for display at all, and a
-- pending rollup_queue item for one would never resolve. Counts are raised as
-- NOTICE so the migration log says what was removed instead of doing it
-- silently.
--
-- Nothing normal produces orphans: classifications and currencies are
-- deactivated (is_active = false), never deleted -- 003's mutation guards
-- make that the only permitted change. A row here therefore means a manual
-- DELETE or a restore that lost dimension rows, which is exactly what the
-- constraint should have been catching.
--
-- NOT VALID first, then VALIDATE CONSTRAINT: adding a validated FK takes an
-- ACCESS EXCLUSIVE lock for the whole scan, while this pair takes the strong
-- lock only for the catalog change and does the scan under SHARE UPDATE
-- EXCLUSIVE, which does not block reads or writes.
DO $$
DECLARE
    orphan_count BIGINT;
BEGIN
    DELETE FROM balance_checkpoints bc
    WHERE NOT EXISTS (SELECT 1 FROM currencies c WHERE c.id = bc.currency_id)
       OR NOT EXISTS (SELECT 1 FROM classifications cl WHERE cl.id = bc.classification_id);
    GET DIAGNOSTICS orphan_count = ROW_COUNT;
    IF orphan_count > 0 THEN
        RAISE NOTICE 'migration 022: deleted % orphan balance_checkpoints row(s); the dimensions will be recomputed from journal_entries on next read', orphan_count;
    END IF;

    DELETE FROM rollup_queue rq
    WHERE NOT EXISTS (SELECT 1 FROM currencies c WHERE c.id = rq.currency_id)
       OR NOT EXISTS (SELECT 1 FROM classifications cl WHERE cl.id = rq.classification_id);
    GET DIAGNOSTICS orphan_count = ROW_COUNT;
    IF orphan_count > 0 THEN
        RAISE NOTICE 'migration 022: deleted % orphan rollup_queue item(s) that could never have resolved', orphan_count;
    END IF;

    DELETE FROM balance_snapshots bs
    WHERE NOT EXISTS (SELECT 1 FROM currencies c WHERE c.id = bs.currency_id)
       OR NOT EXISTS (SELECT 1 FROM classifications cl WHERE cl.id = bs.classification_id);
    GET DIAGNOSTICS orphan_count = ROW_COUNT;
    IF orphan_count > 0 THEN
        RAISE NOTICE 'migration 022: deleted % orphan balance_snapshots row(s)', orphan_count;
    END IF;
END $$;

ALTER TABLE balance_checkpoints
    ADD CONSTRAINT fk_checkpoints_currency
        FOREIGN KEY (currency_id) REFERENCES currencies(id) NOT VALID,
    ADD CONSTRAINT fk_checkpoints_classification
        FOREIGN KEY (classification_id) REFERENCES classifications(id) NOT VALID;

ALTER TABLE rollup_queue
    ADD CONSTRAINT fk_rollup_queue_currency
        FOREIGN KEY (currency_id) REFERENCES currencies(id) NOT VALID,
    ADD CONSTRAINT fk_rollup_queue_classification
        FOREIGN KEY (classification_id) REFERENCES classifications(id) NOT VALID;

ALTER TABLE balance_snapshots
    ADD CONSTRAINT fk_snapshots_currency
        FOREIGN KEY (currency_id) REFERENCES currencies(id) NOT VALID,
    ADD CONSTRAINT fk_snapshots_classification
        FOREIGN KEY (classification_id) REFERENCES classifications(id) NOT VALID;

ALTER TABLE balance_checkpoints VALIDATE CONSTRAINT fk_checkpoints_currency;
ALTER TABLE balance_checkpoints VALIDATE CONSTRAINT fk_checkpoints_classification;
ALTER TABLE rollup_queue VALIDATE CONSTRAINT fk_rollup_queue_currency;
ALTER TABLE rollup_queue VALIDATE CONSTRAINT fk_rollup_queue_classification;
ALTER TABLE balance_snapshots VALIDATE CONSTRAINT fk_snapshots_currency;
ALTER TABLE balance_snapshots VALIDATE CONSTRAINT fk_snapshots_classification;
