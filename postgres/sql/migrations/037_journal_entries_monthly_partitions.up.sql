-- Monthly range partitions for journal_entries (I-13 goes from "groundwork"
-- to active): move any rows accumulated in the DEFAULT partition into named
-- monthly partitions, pre-create a rolling horizon, and re-attach an EMPTY
-- default as the safety net. From here the worker's partition job
-- (service/partition.go) keeps the horizon ahead of now.
--
-- The whole DO block is one statement — atomic. The deferred journal-balance
-- constraint trigger re-validates at commit, when every moved journal is
-- complete again.
DO $$
DECLARE
    min_month date;
    max_month date;
    horizon   date := (date_trunc('month', now()) + interval '4 months')::date;
    m         date;
BEGIN
    -- Detach the default partition (if attached) so overlapping monthly
    -- partitions can be created. pg_inherits check makes re-runs safe after
    -- a partial failure.
    IF to_regclass('journal_entries_default') IS NOT NULL
       AND EXISTS (SELECT 1 FROM pg_inherits
                   WHERE inhrelid = 'journal_entries_default'::regclass) THEN
        ALTER TABLE journal_entries DETACH PARTITION journal_entries_default;
    END IF;

    -- Month range that must be covered: existing rows (if any) .. horizon.
    SELECT date_trunc('month', min(created_at))::date,
           date_trunc('month', max(created_at))::date
      INTO min_month, max_month
      FROM journal_entries_default;

    m := LEAST(COALESCE(min_month, date_trunc('month', now())::date),
               date_trunc('month', now())::date);
    max_month := GREATEST(COALESCE(max_month, m), horizon);

    WHILE m <= max_month LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
            format('journal_entries_y%sm%s', to_char(m, 'YYYY'), to_char(m, 'MM')),
            m, (m + interval '1 month')::date);
        m := (m + interval '1 month')::date;
    END LOOP;

    -- Move rows out of the detached default into their monthly homes, then
    -- re-attach the (now empty) default as the catch-all.
    IF min_month IS NOT NULL THEN
        INSERT INTO journal_entries SELECT * FROM journal_entries_default;
        TRUNCATE journal_entries_default;
    END IF;
    ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT;
END $$;
