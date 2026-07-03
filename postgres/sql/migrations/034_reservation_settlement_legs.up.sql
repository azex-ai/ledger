-- Settlement legs: durable idempotency records for SettlePartial (I-3).
-- SettleReservationPartial is a pure accumulator (settled_amount += $x), so a
-- client retry of a lost response used to double-apply the amount. Each
-- partial settlement now inserts a leg row keyed by a caller-supplied
-- idempotency key; a replayed key with the same amount returns success
-- without re-applying, a replayed key with a different amount is ErrConflict.
--
-- Internal-only table: legs never appear in any public contract (no uid
-- column needed — I-18 governs externally visible identifiers).
CREATE TABLE IF NOT EXISTS reservation_settlement_legs (
    id              BIGSERIAL PRIMARY KEY,
    reservation_id  BIGINT NOT NULL REFERENCES reservations(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    amount          NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_settlement_legs_reservation
    ON reservation_settlement_legs (reservation_id);
