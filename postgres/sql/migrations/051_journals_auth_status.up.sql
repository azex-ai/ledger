-- 051_journals_auth_status.up.sql
--
-- P5 follow-up fix (docs/plans/2026-08-21-tamper-evident-ledger-design.md
-- §7.5, board #12/#13): P5 (migration 046) added auth_digest/auth_signature/
-- auth_key_id, but left "why are these empty" unrecorded. That made three
-- very different situations byte-for-byte indistinguishable in the data:
--   (a) no core.Attestor is configured for this deployment at all;
--   (b) the journal was posted through a write path with no safe point to
--       call an Attestor without violating financial.md's "no external
--       calls inside a transaction" rule (PostJournal's tx-mode branch,
--       ExecuteTemplateBatch, reversals) -- including, critically,
--       service/onchain.go's postDepositConfirmedJournal, composed via
--       ledger.Service.RunInTx, which is P5's own headline use case (M5:
--       forged deposit accounting);
--   (c) a forged row inserted directly by SQL, bypassing the application
--       entirely.
--
-- auth_status makes (a) and (b) explicit, queryable facts instead of
-- something a verifier has to infer from three possibly-empty byte columns.
-- (c) remains indistinguishable from (a)/(b) by this column alone -- an
-- attacker with direct SQL access can simply write whichever auth_status
-- string they like; this column is a debugging/audit aid for the
-- legitimate-write-path cases, not a defense against SQL-level forgery
-- (that defense is per-journal signing itself, verified out-of-band via
-- core.VerifyJournalAuth, not this column -- see design doc §1 non-goal 2
-- and §7.4).
ALTER TABLE journals
    ADD COLUMN IF NOT EXISTS auth_status TEXT NOT NULL DEFAULT 'unsigned_no_attestor'
    CHECK (auth_status IN ('signed', 'unsigned_no_attestor', 'unsigned_tx_mode'));

-- Backfill: every row that predates this column but already carries a real
-- signature (P5, migration 046) was, in fact, signed -- correct the
-- blanket default rather than mislabeling those rows unsigned_no_attestor.
-- The journals anti-tamper guard (ledger_journals_block_arbitrary_update,
-- installed generic by migration 045, contracts §2) rejects any UPDATE
-- that is not the event_id set-once backfill, so it must be disabled for
-- this one-time schema-migration correction, same idiom as migration 025's
-- effective_at backfill.
ALTER TABLE journals DISABLE TRIGGER journals_no_arbitrary_update;
UPDATE journals
SET auth_status = 'signed'
WHERE auth_signature != ''::bytea
  AND auth_key_id != ''
  AND auth_status <> 'signed';
ALTER TABLE journals ENABLE TRIGGER journals_no_arbitrary_update;

-- Nothing to change in ledger_journals_block_arbitrary_update(): contracts
-- §2's generic to_jsonb(OLD)/to_jsonb(NEW) comparison protects any column
-- not in its explicit mutable whitelist (currently just event_id) by
-- default, and auth_status is not on that list -- it is written once at
-- INSERT and never updated by application code again.
