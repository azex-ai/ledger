-- Closes board #37 / the Minor the 2026-08-25 financial-engineering audit
-- flagged as schema-fact CONFIRMED, consequence PLAUSIBLE
-- (postgres/sql/migrations/001_baseline.up.sql:280-284,
-- financial-correctness.md): journal_entries' primary key is
-- (id, created_at), not id alone, because a partitioned table's primary key
-- must include the partition key. 001_baseline's own comment calls this "a
-- uniqueness backstop beyond trusting the sequence" -- it is not one. The
-- composite key only forbids the same id repeating inside the SAME monthly
-- partition; nothing in the schema stops the same id from appearing once
-- per partition.
--
-- postgres/journal_entry_id_uniqueness_test.go turned the PLAUSIBLE
-- consequence into a CONFIRMED one before this migration existed: every
-- per-account balance path filters strictly on `id > checkpoint.
-- last_entry_id` (I-5), so a row whose id duplicates one already below the
-- watermark is permanently invisible to GetBalance, while
-- SumGlobalDebitCreditByCurrency and reconcile.sql's global check sum every
-- row unfiltered and count it anyway -- and because the forged pair is
-- itself balanced, the global debit==credit sanity check stays green
-- throughout. The two views of the same ledger diverge without either one
-- individually looking wrong.
--
-- The realistic trigger is not "an attacker guesses a free id" -- ledger_app
-- already holds INSERT on journal_entries (001 section 14 classifies it
-- append-only: SELECT/INSERT, no UPDATE), and until this migration that
-- INSERT grant was table-level, which covers every column, id included. A
-- raw INSERT under a leaked ledger_app credential, or a sequence that
-- regresses after a PITR restore and starts re-issuing ids the table has
-- already seen in an older partition, both land here.
--
-- ####  The fix: the sequence becomes the only source of id  ####
--
-- ledger_app's table-level INSERT on journal_entries is replaced with a
-- column-level INSERT that omits id. Postgres then refuses, at the ACL
-- layer (permission denied, SQLSTATE 42501), any INSERT statement whose
-- column list names id explicitly -- regardless of what value it supplies,
-- forged duplicate or not. Every statement that omits id -- which is all of
-- them; postgres/sql/queries/journals.sql's InsertJournalEntry (the only
-- production write path into this table) never lists id -- is unaffected:
-- the column receives its DEFAULT (nextval on the single, table-wide
-- journal_entries_id_seq, shared by every partition), which becomes the
-- only path left standing.
--
-- A trigger that unconditionally overwrote NEW.id with a fresh nextval() was
-- considered and rejected: by the time a BEFORE INSERT trigger fires,
-- Postgres has already applied column defaults to build the candidate row,
-- so the trigger cannot distinguish "caller omitted id, default applied" from
-- "caller supplied an explicit id" -- it would silently substitute a value
-- rather than reject the statement, which does not match this migration's
-- attack test (an explicit-id INSERT under ledger_app must fail, not
-- silently succeed with a different id).
--
-- ####  Why every partition, derived from the catalog  ####
--
-- journal_entries is partitioned, and a GRANT/REVOKE issued on the parent
-- does not reach an already-existing partition -- section 14's own comment
-- says so, which is why its ACL-derivation loop grants each partition
-- individually. Every partition that existed at 001_baseline install time
-- (the bootstrap four-month horizon plus journal_entries_default) therefore
-- still carries that same table-level INSERT today, id included, regardless
-- of what the parent's own grant says -- fixing only the parent would leave
-- those specific partitions reachable by name
-- (`INSERT INTO journal_entries_y2026m03 (id, ...) ...`) with the gap this
-- migration exists to close still open on them.
--
-- pg_partition_tree('journal_entries') returns the parent plus every
-- partition that exists right now, so the loop below closes the id column on
-- whichever ones a given deployment has actually accumulated without this
-- file having to name them -- the same "derive from the catalog, not a
-- list" discipline 001 section 14 and 003/006's guard classification already
-- use.
--
-- It deliberately does NOT need to (and cannot) reach partitions created
-- after this migration runs. Every partition created since migration 007
-- goes through ledger_create_monthly_partition / ledger_rebalance_default_partition,
-- both SECURITY DEFINER and neither one issuing ledger_app any grant on the
-- new partition itself -- confirmed by
-- postgres.TestLedgerAppInsertsIntoPartitionCreatedAfterGrant, which inserts
-- into a brand-new partition through ledger_app with no per-partition GRANT
-- ever issued. The only way ledger_app reaches such a partition at all is
-- tuple-routing through the parent's own name, and Postgres checks INSERT
-- privilege for a routed insert against the table named in the statement
-- (the parent), not the partition the row physically lands in -- which is
-- exactly what that same pin proves and exactly what this migration's grant
-- on the parent governs going forward.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_partition_tree('journal_entries'::regclass) pt
        JOIN pg_class c ON c.oid = pt.relid
    LOOP
        EXECUTE format('REVOKE INSERT ON public.%I FROM ledger_app', r.relname);
        EXECUTE format(
            'GRANT INSERT (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at) ON public.%I TO ledger_app',
            r.relname);
    END LOOP;
END $$;
