-- name: InsertBookingTransitionReceipt :one
-- Durable idempotency record for one Transition application when the caller
-- opted in via TransitionInput.IdempotencyKey (I-3; see that field's doc
-- comment for why it is opt-in). Mirrors InsertReservationOperationReceipt's
-- pattern: on a replayed key this inserts nothing and returns no row, and
-- the caller then fetches the existing receipt and compares payloads.
INSERT INTO booking_transition_receipts (booking_id, idempotency_key, to_status, channel_ref, amount, event_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id, booking_id, idempotency_key, to_status, channel_ref, amount, event_id, created_at;

-- name: GetBookingTransitionReceiptByIdempotencyKey :one
SELECT id, booking_id, idempotency_key, to_status, channel_ref, amount, event_id, created_at
FROM booking_transition_receipts
WHERE idempotency_key = $1;
