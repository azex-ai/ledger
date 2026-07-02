-- name: InsertReservation :one
INSERT INTO reservations (account_holder, currency_id, reserved_amount, idempotency_key, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at;

-- name: GetReservation :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE id = $1;

-- name: GetReservationByIdempotencyKey :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE idempotency_key = $1;

-- name: GetReservationForUpdate :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE id = $1 FOR UPDATE;

-- name: UpdateReservationStatus :exec
UPDATE reservations SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateReservationSettle :exec
UPDATE reservations SET status = 'settled', settled_amount = $2, journal_id = $3, updated_at = now() WHERE id = $1;

-- name: SettleReservationPartial :exec
-- Accumulates settled_amount (unlike UpdateReservationSettle, which overwrites
-- it) and moves status to 'settling' — a no-op status change if it's already
-- there. Caller (ReserverStore.SettlePartial) has already row-locked this
-- reservation via GetReservationForUpdate and verified the cumulative amount
-- stays within reserved_amount; chk_settled_lte_reserved is the DB-level backstop.
UPDATE reservations SET status = 'settling', settled_amount = settled_amount + $2, updated_at = now() WHERE id = $1;

-- name: FinalizeReservationSettlement :exec
-- Moves a 'settling' reservation to 'settled' without touching settled_amount
-- — the remaining (reserved_amount - settled_amount) is implicitly released,
-- same as the one-shot Settle's unused-remainder semantics.
UPDATE reservations SET status = 'settled', updated_at = now() WHERE id = $1;

-- name: ListReservationsByAccount :many
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations
WHERE (sqlc.arg(account_holder)::bigint = 0 OR account_holder = sqlc.arg(account_holder))
  AND (sqlc.arg(filter_status)::text = '' OR status = sqlc.arg(filter_status))
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetExpiredReservations :many
-- Includes 'settling' alongside 'active': a partially-settled reservation
-- that expires must still be wound down (auto-finalized, keeping the settled
-- portion and releasing the rest — see service.ExpirationService), not left
-- dangling forever. NB: idx_reservations_expired is a partial index WHERE
-- status = 'active', so this query no longer hits it for 'settling' rows;
-- acceptable at current scale (see docs/plans/2026-07-02-financial-core-hardening-design.md §5b).
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at
FROM reservations WHERE status IN ('active', 'settling') AND expires_at < now()
ORDER BY expires_at ASC
LIMIT $1;

-- name: CountActiveReservations :one
SELECT COUNT(*) FROM reservations WHERE status = 'active';

-- name: SumActiveReservations :one
-- Outstanding hold across the holder's not-yet-terminal reservations. An
-- 'active' reservation holds its full reserved_amount; a 'settling' one
-- (partially settled via SettlePartial) still holds the unsettled remainder
-- (reserved - settled) — counting it as zero would let a concurrent Reserve
-- over-commit the balance the moment the first partial settlement lands
-- (the exact TOCTOU class I-4/I-11 exist to prevent). NB: the partial index
-- idx_reservations_account_status covers only status='active'; the extra
-- 'settling' rows are expected to be few at any moment.
SELECT COALESCE(SUM(
    CASE WHEN status = 'active' THEN reserved_amount
         ELSE reserved_amount - settled_amount
    END), 0) as total
FROM reservations
WHERE account_holder = $1 AND currency_id = $2 AND status IN ('active', 'settling');
