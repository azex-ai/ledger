-- Wave 3 adversarial re-review, 2026-09-02 (w3-review/money-path.md m-4).
--
-- ####  What this migration does NOT do: m-3  ####
--
-- The same review reported that ledger_app can widen the withdrawal gate's
-- spendable base with `UPDATE classifications SET balance_role='available'`
-- on a role-less classification holding a real, signed balance, citing
-- 003_config_table_guards.up.sql:111-135. That reading stops one migration
-- short: 004_refuse_balance_role_promotion_with_history REPLACED that
-- function, and the replacement refuses exactly this UPDATE when the
-- classification has any journal_entries row -- which is the only shape in
-- which the promotion moves money. It was verified as ledger_app before this
-- migration was written (postgres/migration_024_test.go's
-- TestBalanceRolePromotion_RefusedForLedgerApp) and left alone. Blanket-
-- banning '' -> 'available' would have broken the install-time upgrade three
-- existing pins require, for no security gain.
--
-- ####  m-4: a forged anchor observation is unrecoverable  ####
--
-- 018 gave ledger_app SELECT + INSERT on anchor_observations, with
-- append-only triggers. The append-only half works exactly as designed: the
-- only read is MAX(observed_seq), so the DANGEROUS direction (making a
-- rollback look like progress) is closed -- an attacker cannot lower the
-- remembered head.
--
-- The other direction was open and permanent:
--
--     INSERT INTO anchor_observations (uid, observed_seq, observed_head)
--     VALUES (gen_random_uuid(), 999999, ''::bytea);
--     -- measured: succeeded as ledger_app
--
-- after which service.VerifyLedger's `anchorSeq < lastObserved` is true on
-- every future run, forever: TAMPERED with no forensic content, on a table
-- that refuses UPDATE and DELETE to everyone, with no runbook path back.
-- Fail-closed is right; a red light one INSERT can weld on is not.
--
-- Fix, in the same shape 020 used for the audit tables: the legitimate write
-- becomes a SECURITY DEFINER function, direct INSERT is revoked, and the
-- function enforces the one property a memory of "what the anchor said" can
-- be checked against locally -- it cannot be ahead of this deployment's own
-- attestation chain. An anchor reporting a seq the DB has never produced is
-- not an observation worth remembering; VerifyLedger's own
-- `anchorSeq > maxSeqSeen` check reports that situation as TAMPERED on the
-- spot, and does not need a permanent record to do it.
--
-- What that buys: the worst a leaked credential can now record is the true
-- current chain height. The anchor catches up to that height on its own
-- (AttestationService.catchUpAnchor republishes the gap), so the false red
-- HEALS instead of having to be surgically removed from an append-only
-- table. seq 0 stays legal and meaningful ("the anchor was reachable and
-- reported empty") -- that row is what makes a later rollback provable.

------------------------------------------------------------
-- anchor_observations: owner-written, and never ahead of the chain.
------------------------------------------------------------

CREATE FUNCTION ledger_record_anchor_observation(p_uid uuid, p_seq bigint, p_head bytea) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    chain_head BIGINT;
BEGIN
    IF p_seq < 0 THEN
        RAISE EXCEPTION 'ledger: anchor observation seq must be >= 0, got %', p_seq
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT COALESCE(MAX(seq), 0) INTO chain_head FROM ledger_attestations;

    IF p_seq > chain_head THEN
        RAISE EXCEPTION 'ledger: refusing to record an anchor observation at seq % while this deployment''s own attestation chain only reaches seq % -- an anchor ahead of the local chain is reported by service.VerifyLedger as tamper evidence on the spot and must not be written into the permanent memory that decides every future run',
            p_seq, chain_head
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO anchor_observations (uid, observed_seq, observed_head)
    VALUES (p_uid, p_seq, p_head);
END;
$$;

ALTER FUNCTION ledger_record_anchor_observation(uuid, bigint, bytea) OWNER TO ledger_owner;

REVOKE ALL ON FUNCTION ledger_record_anchor_observation(uuid, bigint, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_record_anchor_observation(uuid, bigint, bytea) TO ledger_app;

REVOKE INSERT ON public.anchor_observations FROM ledger_app;
REVOKE USAGE, SELECT ON SEQUENCE public.anchor_observations_id_seq FROM ledger_app;
