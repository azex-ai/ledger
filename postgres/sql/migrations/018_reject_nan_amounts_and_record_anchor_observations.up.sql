-- Two integrity holes the 2026-09-02 deep audit measured, both in the
-- tamper-evidence territory (docs/audits/2026-09-02-deep-audit/
-- tamper-evident.md M-7 and M-3).
--
-- ============================================================================
-- 1. NaN is not a legal amount (M-7 / C-M7)
-- ============================================================================
--
-- NUMERIC accepts the special value 'NaN', and none of this schema's existing
-- amount guards reject it. Measured on PostgreSQL 18 against a column
-- declared exactly like journal_entries.amount:
--
--     CREATE TABLE t (a NUMERIC(30,18) NOT NULL CHECK (a > 0));
--     INSERT INTO t VALUES ('NaN');           -- INSERT 0 1  (accepted!)
--     SELECT a, a = a, a > 0 FROM t;          -- NaN | t | t
--
-- Both of the obvious defences are no defence at all: on numeric, NaN = NaN
-- is TRUE (so a `CHECK (a = a)` self-equality test passes) and NaN sorts
-- ABOVE every finite value (so `CHECK (a > 0)` passes too). 'Infinity' is
-- already rejected, but by the typmod rather than by any CHECK
-- ("numeric field overflow: a field with precision 30, scale 18 cannot hold
-- an infinite value"), so only NaN needs a constraint.
--
-- What one such row buys an attacker holding the application credential:
-- postgres.mustNumericToDecimal PANICS on a NaN numeric, and it sits on the
-- read path of service.VerifyLedger's journal sampling, the reconcile suite,
-- and every journal read. One row turned the whole verification side and the
-- worker process off. The read side is being made fail-closed in the same
-- change (postgres/convert.go now propagates the error instead of panicking),
-- but the durable fix is not accepting the value in the first place.
--
-- The check is written as `<col>::text <> 'NaN'` -- the only formulation that
-- works, for the reasons above. numeric_out is immutable, so it is legal in a
-- CHECK constraint.
--
-- Coverage is EVERY NUMERIC column in the schema, not just the ones the audit
-- named (remediation contract §0: fix the whole shape, not the reported
-- instance). The list below is `grep -n NUMERIC postgres/sql/migrations/
-- *.up.sql` in full: 20 columns across 15 tables.
--
-- deposits.actual_amount is the one nullable column in the set; a CHECK is
-- satisfied by NULL, so it needs no special handling.
--
-- ⚠️ Operational note for a large existing deployment: each ALTER TABLE below
-- takes ACCESS EXCLUSIVE and validates every existing row. On a
-- journal_entries with hundreds of millions of rows, split each one into
-- `ADD CONSTRAINT ... NOT VALID` followed by a separate `VALIDATE CONSTRAINT`
-- (which takes only SHARE UPDATE EXCLUSIVE). This file does it in one shot
-- deliberately: the constraint must be true, not merely declared, before
-- anything downstream is allowed to trust it.
--
-- ============================================================================
-- 2. anchor_observations: making anchor REGRESSION detectable (M-3 / C-M3)
-- ============================================================================
--
-- core.Anchor.Head returns "the highest seq the anchor knows about, or 0 if
-- empty" -- so "nothing was ever published" and "what was published has been
-- erased or rolled back" are the SAME observation. service.VerifyLedger read
-- both as DRIFT, which its own doc comment defined as a benign, expected
-- inconsistency, and ledger-cli exited 0. Deleting the anchor was therefore a
-- silent way to switch every external check off.
--
-- Detecting a rollback requires remembering what the anchor said last time.
-- That memory cannot live in the anchor (the thing under suspicion), so it
-- lives here, append-only: one row per observation, written by the
-- attestation job each time it successfully reads the anchor's head.
-- VerifyLedger compares the live head against MAX(observed_seq): lower than
-- something we recorded seeing is TAMPERED, not DRIFT.
--
-- Threat-model scope, stated plainly: a party who can rewrite this table can
-- also erase the memory. That party is the same DB-credential holder P6's
-- external anchor exists to defend against, and the anchor still holds the
-- evidence on its side. What this table closes is the asymmetry where the
-- attacker had to compromise NOTHING in the database to make an erased anchor
-- look benign.
--
-- Shape follows checkpoint_rebuilds (001): append-only via
-- ledger_block_mutation(), a uid for external reference, and no UPDATE grant.

------------------------------------------------------------------------------
-- 0. Temporary ownership membership.
--
-- Every table altered below is owned by ledger_owner (001 §14 transferred
-- them), and ALTER TABLE ... ADD CONSTRAINT is an owner-gated action. 001
-- deliberately REVOKEs the runner's ledger_owner membership when it finishes,
-- so a later migration has to re-take it for the length of its own
-- transaction -- the same "keepsake" shape 001 uses for its schema_migrations
-- re-grant. The membership is dropped again at the end of this file.
--
-- Migration 016 got away with a bare `ALTER TABLE ... DISABLE TRIGGER` only
-- because the runner in our own test and dev setups happens to be a
-- superuser; that is exactly the assumption threat-model.md flags as
-- untested. Taking the membership explicitly makes this file work under a
-- NOSUPERUSER bootstrap credential too.
------------------------------------------------------------------------------

DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('GRANT ledger_owner TO %I WITH INHERIT TRUE', runner);
END $$;

------------------------------------------------------------------------------
-- 1. NaN rejection
------------------------------------------------------------------------------

-- Refuse to install the constraints while a violating row exists, naming the
-- table so an operator knows where to look. journals and journal_entries are
-- append-only, so a NaN row there has to be corrected by a reversal posting
-- (financial.md), not by an UPDATE this migration could do for them.
DO $$
DECLARE
    r        record;
    offenders text := '';
    n        bigint;
BEGIN
    FOR r IN
        SELECT * FROM (VALUES
            ('journals', 'total_debit'),
            ('journals', 'total_credit'),
            ('journal_entries', 'amount'),
            ('balance_checkpoints', 'balance'),
            ('balance_snapshots', 'balance'),
            ('system_rollups', 'total_balance'),
            ('reservations', 'reserved_amount'),
            ('reservations', 'settled_amount'),
            ('reservation_settlement_legs', 'amount'),
            ('bookings', 'amount'),
            ('bookings', 'settled_amount'),
            ('events', 'amount'),
            ('events', 'settled_amount'),
            ('deposits', 'expected_amount'),
            ('deposits', 'actual_amount'),
            ('withdrawals', 'amount'),
            ('account_policies', 'min_balance'),
            ('checkpoint_rebuilds', 'previous_balance'),
            ('checkpoint_rebuilds', 'new_balance'),
            ('checkpoint_rebuilds', 'drift'),
            ('reservation_operation_receipts', 'amount'),
            ('booking_transition_receipts', 'amount')
        ) AS v(tbl, col)
    LOOP
        EXECUTE format('SELECT count(*) FROM public.%I WHERE %I::text = %L', r.tbl, r.col, 'NaN') INTO n;
        IF n > 0 THEN
            offenders := offenders || format('%s.%s: %s row(s); ', r.tbl, r.col, n);
        END IF;
    END LOOP;

    IF offenders <> '' THEN
        RAISE EXCEPTION 'migration 018: existing NaN amounts must be corrected before this constraint can be installed: %', offenders
            USING HINT = 'journals/journal_entries are append-only: correct by posting a reversal, then re-run. Cache tables (balance_checkpoints, balance_snapshots, system_rollups) can be deleted and will be recomputed.';
    END IF;
END $$;

ALTER TABLE journals
    ADD CONSTRAINT chk_journals_total_debit_not_nan  CHECK (total_debit::text  <> 'NaN'),
    ADD CONSTRAINT chk_journals_total_credit_not_nan CHECK (total_credit::text <> 'NaN');

-- Declared on the partitioned parent so it propagates to every existing and
-- future partition (including journal_entries_default and the partitions the
-- worker's partition job creates later).
ALTER TABLE journal_entries
    ADD CONSTRAINT chk_journal_entries_amount_not_nan CHECK (amount::text <> 'NaN');

ALTER TABLE balance_checkpoints
    ADD CONSTRAINT chk_balance_checkpoints_balance_not_nan CHECK (balance::text <> 'NaN');

ALTER TABLE balance_snapshots
    ADD CONSTRAINT chk_balance_snapshots_balance_not_nan CHECK (balance::text <> 'NaN');

ALTER TABLE system_rollups
    ADD CONSTRAINT chk_system_rollups_total_balance_not_nan CHECK (total_balance::text <> 'NaN');

ALTER TABLE reservations
    ADD CONSTRAINT chk_reservations_reserved_amount_not_nan CHECK (reserved_amount::text <> 'NaN'),
    ADD CONSTRAINT chk_reservations_settled_amount_not_nan  CHECK (settled_amount::text  <> 'NaN');

ALTER TABLE reservation_settlement_legs
    ADD CONSTRAINT chk_reservation_settlement_legs_amount_not_nan CHECK (amount::text <> 'NaN');

ALTER TABLE bookings
    ADD CONSTRAINT chk_bookings_amount_not_nan         CHECK (amount::text <> 'NaN'),
    ADD CONSTRAINT chk_bookings_settled_amount_not_nan CHECK (settled_amount::text <> 'NaN');

ALTER TABLE events
    ADD CONSTRAINT chk_events_amount_not_nan         CHECK (amount::text <> 'NaN'),
    ADD CONSTRAINT chk_events_settled_amount_not_nan CHECK (settled_amount::text <> 'NaN');

ALTER TABLE deposits
    ADD CONSTRAINT chk_deposits_expected_amount_not_nan CHECK (expected_amount::text <> 'NaN'),
    ADD CONSTRAINT chk_deposits_actual_amount_not_nan   CHECK (actual_amount::text   <> 'NaN');

ALTER TABLE withdrawals
    ADD CONSTRAINT chk_withdrawals_amount_not_nan CHECK (amount::text <> 'NaN');

ALTER TABLE account_policies
    ADD CONSTRAINT chk_account_policies_min_balance_not_nan CHECK (min_balance::text <> 'NaN');

ALTER TABLE checkpoint_rebuilds
    ADD CONSTRAINT chk_checkpoint_rebuilds_previous_balance_not_nan CHECK (previous_balance::text <> 'NaN'),
    ADD CONSTRAINT chk_checkpoint_rebuilds_new_balance_not_nan      CHECK (new_balance::text      <> 'NaN'),
    ADD CONSTRAINT chk_checkpoint_rebuilds_drift_not_nan            CHECK (drift::text            <> 'NaN');

ALTER TABLE reservation_operation_receipts
    ADD CONSTRAINT chk_reservation_operation_receipts_amount_not_nan CHECK (amount::text <> 'NaN');

ALTER TABLE booking_transition_receipts
    ADD CONSTRAINT chk_booking_transition_receipts_amount_not_nan CHECK (amount::text <> 'NaN');

------------------------------------------------------------------------------
-- 2. anchor_observations
------------------------------------------------------------------------------

CREATE TABLE anchor_observations (
    id            BIGSERIAL PRIMARY KEY,
    uid           UUID   NOT NULL,
    -- The seq core.Anchor.Head returned. 0 is legal and meaningful: it
    -- records "the anchor was reachable and reported empty", which is what
    -- makes a later 0 after a non-zero observation provable evidence rather
    -- than an indistinguishable first read.
    observed_seq  BIGINT NOT NULL CHECK (observed_seq >= 0),
    observed_head BYTEA  NOT NULL DEFAULT ''::bytea,
    observed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_anchor_observations_uid ON anchor_observations (uid);
-- MAX(observed_seq) is the only read this table has.
CREATE INDEX idx_anchor_observations_seq ON anchor_observations (observed_seq DESC);

CREATE TRIGGER anchor_observations_no_update
    BEFORE UPDATE ON anchor_observations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER anchor_observations_no_delete
    BEFORE DELETE ON anchor_observations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

-- No UPDATE, no DELETE: append-only in the ACL as well as in the trigger.
GRANT SELECT, INSERT ON public.anchor_observations TO ledger_app;
GRANT USAGE, SELECT ON public.anchor_observations_id_seq TO ledger_app;
-- The verification side reads it (service.VerifyLedger), and per
-- docs/RUNBOOK.md that side runs with the read-only credential.
GRANT SELECT ON public.anchor_observations TO ledger_ro;
GRANT SELECT ON public.anchor_observations_id_seq TO ledger_ro;

-- Ownership. Migrations 002-017 all left their objects owned by the
-- migration runner rather than ledger_owner (001's ownership sweep was a
-- one-time loop -- threat-model.md's finding); this one does not. Grants
-- first, then the transfer, for the reason 001 §14 spells out: after the
-- transfer the runner no longer owns the object and cannot grant on it.
ALTER TABLE    public.anchor_observations        OWNER TO ledger_owner;
ALTER SEQUENCE public.anchor_observations_id_seq OWNER TO ledger_owner;

------------------------------------------------------------------------------
-- 3. Drop the temporary membership taken in section 0.
------------------------------------------------------------------------------

DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;
