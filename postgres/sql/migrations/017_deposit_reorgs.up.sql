-- deposit_reorgs: the durable half of reorg handling on the deposit path
-- (2026-09-02 deep audit, onchain-money-path.md G-M8 / G-M1).
--
-- Before this table, a deep reorg under the default ReorgPolicyManual left
-- exactly two traces: a Warn log line (which lands in core.NopLogger() unless
-- the consumer injected a logger) and a Prometheus counter increment. The
-- recheck that produced them stopped repeating as soon as the booking's block
-- fell outside service.Onchain's reorgRecheckWindow -- 500 blocks, i.e. about
-- 17 minutes on a 2-second chain, shorter than an on-call response time. So
-- the signal that told an operator WHICH booking to reverse expired before
-- they could act on RUNBOOK §12, and nothing in the system afterwards
-- remembered it had happened.
--
-- One row per (booking, kind). Rows are never deleted; the only mutation is
-- bumping last_seen_at while the anomaly is still observable, or closing it
-- out with resolved_at + resolution. service.Onchain also keeps rechecking
-- any booking with an OPEN row regardless of the recheck window, so "still
-- true" and "nobody has looked yet" stay distinguishable
-- (working-agreements §3).
--
-- No-NULL convention (CLAUDE.md): resolved_at defaults to the epoch, which
-- core.DepositReorg.IsOpen reads as "open"; journal_uid is '' for the
-- shallow-reorg kind, which by definition never posted one.
--
-- ⚠️ Ownership: like every object created by migrations 002-016, this table is
-- owned by the migration runner, not by ledger_owner (001's ownership sweep
-- was a one-time loop -- threat-model.md's finding, being fixed under
-- D-threat). It must be picked up by that sweep; the grants below are the
-- same least-privilege set 005/006's tables use in the meantime.
CREATE TABLE deposit_reorgs (
    id          BIGSERIAL PRIMARY KEY,
    uid         UUID NOT NULL,
    kind        TEXT NOT NULL
                CHECK (kind IN ('deep_reorg', 'shallow_reorg_failed')),
    booking_uid UUID NOT NULL,
    chain_id    BIGINT NOT NULL CHECK (chain_id > 0),
    tx_hash     TEXT NOT NULL,
    journal_uid TEXT NOT NULL DEFAULT '',
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    resolution  TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX uq_deposit_reorgs_uid            ON deposit_reorgs (uid);
CREATE UNIQUE INDEX uq_deposit_reorgs_booking_kind   ON deposit_reorgs (booking_uid, kind);
CREATE INDEX idx_deposit_reorgs_open                 ON deposit_reorgs (detected_at DESC)
    WHERE resolved_at <= 'epoch';

GRANT SELECT, INSERT, UPDATE ON public.deposit_reorgs TO ledger_app;
GRANT USAGE, SELECT ON public.deposit_reorgs_id_seq TO ledger_app;
GRANT SELECT ON public.deposit_reorgs TO ledger_ro;
GRANT SELECT ON public.deposit_reorgs_id_seq TO ledger_ro;
