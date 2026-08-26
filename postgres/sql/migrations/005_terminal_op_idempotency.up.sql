-- Idempotency keys for the reservation and booking transitions that end a
-- state machine at a terminal state.
--
-- I-3 promises "every state-changing operation requires an idempotency_key.
-- Replaying the same key with the same payload returns the original result".
-- reservations.idempotency_key and bookings.idempotency_key only cover the
-- CREATE step (Reserve / CreateBooking). Settle, Release and
-- FinalizeSettlement all move a reservation into a terminal status (settled
-- or released) with no key at all: a lost-response retry re-runs the same
-- status-machine check, finds the row already terminal, and returns
-- ErrInvalidTransition -- indistinguishable from a genuine conflict (someone
-- else settling a different amount, or the reservation having been released
-- out from under the caller in the meantime).
--
-- This follows the same shape reservation_settlement_legs (001_baseline)
-- already established for SettlePartial: a side table keyed by
-- idempotency_key, checked under the reservation's row lock, so a replay
-- short-circuits to success and a payload mismatch is ErrConflict --
-- rather than a nullable idempotency column per operation on `reservations`
-- itself, which would need three separate columns (settle / release /
-- finalize_settlement) to avoid conflating operations that reach the same
-- terminal status via different paths.
--
-- reservation_operation_receipts is one row per successfully-applied
-- Settle/Release/FinalizeSettlement call. `operation` records which one, so
-- reusing a key for a different operation on the same reservation is a
-- payload mismatch (ErrConflict), not a silent success. `amount` is only
-- meaningful for 'settle' (the actual amount settled); it is 0 for 'release'
-- and 'finalize_settlement', which take no amount.
CREATE TABLE reservation_operation_receipts (
    id              BIGSERIAL PRIMARY KEY,
    reservation_id  BIGINT NOT NULL REFERENCES reservations(id),
    operation       TEXT NOT NULL CHECK (operation IN ('settle', 'release', 'finalize_settlement')),
    idempotency_key TEXT UNIQUE NOT NULL,
    amount          NUMERIC(30,18) NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_reservation_operation_receipts_reservation ON reservation_operation_receipts (reservation_id);

GRANT SELECT, INSERT, UPDATE ON public.reservation_operation_receipts TO ledger_app;
GRANT SELECT ON public.reservation_operation_receipts TO ledger_ro;
GRANT USAGE, SELECT ON public.reservation_operation_receipts_id_seq TO ledger_app;
GRANT SELECT ON public.reservation_operation_receipts_id_seq TO ledger_ro;

-- booking_transition_receipts is Transition's OPT-IN counterpart (see
-- core.TransitionInput.IdempotencyKey doc comment for why it is opt-in
-- rather than mandatory: Transition already has a narrower,
-- state-comparison-based idempotency path that covers most system-driven
-- callers with no natural request-scoped key, and this table only backs
-- callers that explicitly set the field). event_id links a replay back to
-- the event Transition originally returned, so a repeated call with the same
-- key returns that same *core.Event rather than re-deriving one.
CREATE TABLE booking_transition_receipts (
    id              BIGSERIAL PRIMARY KEY,
    booking_id      BIGINT NOT NULL REFERENCES bookings(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    to_status       TEXT NOT NULL,
    channel_ref     TEXT NOT NULL DEFAULT '',
    amount          NUMERIC(30,18) NOT NULL DEFAULT 0,
    event_id        BIGINT NOT NULL REFERENCES events(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_booking_transition_receipts_booking ON booking_transition_receipts (booking_id);

GRANT SELECT, INSERT, UPDATE ON public.booking_transition_receipts TO ledger_app;
GRANT SELECT ON public.booking_transition_receipts TO ledger_ro;
GRANT USAGE, SELECT ON public.booking_transition_receipts_id_seq TO ledger_app;
GRANT SELECT ON public.booking_transition_receipts_id_seq TO ledger_ro;
