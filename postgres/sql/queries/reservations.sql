-- name: InsertReservation :one
INSERT INTO reservations (account_holder, currency_id, reserved_amount, idempotency_key, expires_at, uid)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid;

-- name: GetReservation :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
FROM reservations WHERE id = $1;

-- name: GetReservationByIdempotencyKey :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
FROM reservations WHERE idempotency_key = $1;

-- name: GetReservationForUpdate :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
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
-- Keyset pagination on id DESC (api-contract §6): before_id = 0 means first
-- page; the caller encodes the last row's id as the opaque next_cursor.
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
FROM reservations
WHERE (sqlc.arg(account_holder)::bigint = 0 OR account_holder = sqlc.arg(account_holder))
  AND (sqlc.arg(filter_status)::text = '' OR status = sqlc.arg(filter_status))
  AND (sqlc.arg(before_id)::bigint = 0 OR id < sqlc.arg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetExpiredReservations :many
-- Includes 'settling' alongside 'active': a partially-settled reservation
-- that expires must still be wound down (auto-finalized, keeping the settled
-- portion and releasing the rest — see service.ExpirationService), not left
-- dangling forever. NB: idx_reservations_expired is a partial index WHERE
-- status = 'active', so this query no longer hits it for 'settling' rows;
-- acceptable at current scale (see docs/plans/2026-07-02-financial-core-hardening-design.md §5b).
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
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

-- name: GetReservationByUID :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
FROM reservations WHERE uid = $1;

-- name: GetReservationForUpdateByUID :one
SELECT id, account_holder, currency_id, reserved_amount, settled_amount, status, journal_id, idempotency_key, expires_at, created_at, updated_at, uid
FROM reservations WHERE uid = $1 FOR UPDATE;

-- name: GetReservationUIDByID :one
SELECT uid FROM reservations WHERE id = $1;

-- name: InsertReservationSettlementLeg :one
-- Durable idempotency record for one SettlePartial application (I-3). On a
-- replayed key this inserts nothing and returns no row; the caller then
-- fetches the existing leg and compares payloads.
--
-- auth_digest / auth_signature / auth_key_id carry the signature over this
-- leg's canonical discharge digest (migration 028, docs/INVARIANTS.md I-65);
-- all three are '' when no Attestor is configured or the caller is inside
-- its own transaction, in which case a gated Reserve keeps holding the whole
-- reservation (I-49's conservative rule).
--
-- created_at is passed rather than defaulted because it is COVERED by that
-- digest: a signature computed over one instant and a row storing a
-- different one can never be re-verified. 'epoch' is the sentinel for "no
-- signed instant, let the database assign it", which keeps the unsigned path
-- byte-for-byte what it was before 028.
INSERT INTO reservation_settlement_legs (reservation_id, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id)
VALUES (
    $1, $2, $3,
    COALESCE(NULLIF(sqlc.arg(recorded_at)::timestamptz, 'epoch'::timestamptz), now()),
    sqlc.arg(auth_digest), sqlc.arg(auth_signature), sqlc.arg(auth_key_id)
)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id, reservation_id, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id;

-- name: GetSettlementLegByIdempotencyKey :one
SELECT id, reservation_id, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id
FROM reservation_settlement_legs
WHERE idempotency_key = $1;

-- name: InsertReservationOperationReceipt :one
-- Durable idempotency record for one Settle/Release/FinalizeSettlement
-- application (I-3), mirroring InsertReservationSettlementLeg's pattern. On
-- a replayed key this inserts nothing and returns no row; the caller then
-- fetches the existing receipt and compares payloads.
--
-- The auth_* columns and the passed-in created_at are exactly
-- InsertReservationSettlementLeg's -- see that query's comment for why the
-- timestamp cannot be left to the column default once it is signed.
INSERT INTO reservation_operation_receipts (reservation_id, operation, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id)
VALUES (
    $1, $2, $3, $4,
    COALESCE(NULLIF(sqlc.arg(recorded_at)::timestamptz, 'epoch'::timestamptz), now()),
    sqlc.arg(auth_digest), sqlc.arg(auth_signature), sqlc.arg(auth_key_id)
)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id, reservation_id, operation, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id;

-- name: GetReservationOperationReceiptByIdempotencyKey :one
SELECT id, reservation_id, operation, idempotency_key, amount, created_at, auth_digest, auth_signature, auth_key_id
FROM reservation_operation_receipts
WHERE idempotency_key = $1;

-- name: SumUnexpiredReservationHolds :one
-- The hold Reserve subtracts when the caller opts into
-- RequireVerifiedBalance (I-49). Deliberately the crudest possible sum: the
-- full reserved_amount of every reservation on the dimension whose
-- expires_at is still in the future, with no credit for anything that claims
-- the reservation is over.
--
-- SumActiveReservations, its ordinary-path twin, reads reservations.status
-- and reservations.settled_amount, and ledger_reservations_guard permits
-- exactly the writes that zero a hold through those columns (active ->
-- settling/settled/released, settled_amount growing) because those are the
-- legitimate transitions. ledger_app holds UPDATE, so one permitted
-- statement made a live 1000 hold report as zero and the gate authorized
-- 2000 against a balance of 1000 (2026-09-02 audit,
-- w3-review/money-path.md C-1).
--
-- Sourcing the discharge from the append-only settlement record instead
-- (reservation_settlement_legs / reservation_operation_receipts) does not
-- fix it: ledger_app must keep INSERT on both -- the application writes them
-- -- so a forged INSERT discharges a hold at the same one-statement cost
-- (measured 2026-09-03). In this threat model the application's credential
-- IS the attacker, so every discharge the application can perform the
-- attacker can perform, and no trigger or ACL can tell them apart.
--
-- expires_at is the one exception, and it is why this query has the shape it
-- does: the guard refuses any UPDATE that changes it (verified: even a
-- superuser statement is rejected), no INSERT can shorten another row's
-- lifetime, and time passing is not a privilege. So "not expired yet" is the
-- only claim about a reservation that a leaked credential cannot manufacture
-- -- and it is therefore the only discharge signal this query accepts.
--
-- Consequence, intended and documented (core.ReserveInput.
-- RequireVerifiedBalance, docs/INVARIANTS.md I-49): a settled or released
-- reservation goes on holding its full reserved_amount under the gate until
-- it expires. The settled portion is double-counted (it already left through
-- its own journal, so E fell too) and a released one is counted at all. Both
-- errors are conservative -- they refuse reservations, never allow them --
-- and both self-heal at expires_at. Callers who need faster recycling set a
-- shorter ExpiresIn.
--
-- now() is the transaction timestamp, i.e. the EARLIEST reading available in
-- this transaction, so a row on the boundary is treated as still held. The
-- settle path uses clock_timestamp() for the mirror-image reason (see
-- ReservationExpiredNow).
SELECT COALESCE(SUM(r.reserved_amount), 0) as total
FROM reservations r
WHERE r.account_holder = $1
  AND r.currency_id = $2
  AND r.expires_at > now();

-- name: ReservationExpiredNow :one
-- Whether this reservation is past its expires_at, evaluated by the database
-- so an application clock that drifts from the database's cannot decide it.
--
-- clock_timestamp(), not now(): now() is fixed at transaction start, and a
-- long-running settlement transaction that began before the expiry would
-- read as not-yet-expired. Taking the LATEST reading makes the settle path
-- refuse as early as possible, which is the opposite corner from
-- SumUnexpiredReservationHolds' now() -- each picks the reading that errs
-- toward "the funds are still held" (I-49).
SELECT (expires_at <= clock_timestamp())::boolean AS expired
FROM reservations WHERE id = $1;

-- name: ListUnexpiredReservationHolds :many
-- The per-reservation form of SumUnexpiredReservationHolds, for the gated
-- path that can subtract a VERIFIED discharge (docs/INVARIANTS.md I-65).
--
-- Same WHERE clause, same trust basis: only columns ledger_reservations_guard
-- refuses to let anyone change (reserved_amount, expires_at, uid) are read.
-- status and settled_amount are deliberately absent, exactly as in the SUM
-- form -- the discharge is decided by signature, not by the state machine.
--
-- Returned per row rather than summed because the caller subtracts a
-- different, individually-verified discharge from each reservation and must
-- floor each difference at zero independently; summing first would let one
-- reservation's over-discharge (only reachable via a claim that fails
-- verification, hence never credited) offset another's hold.
--
-- uid is returned because it is what the discharge digest covers -- the
-- verifier recomputes a uid-space preimage and never needs to know the
-- BIGSERIAL id, which is only used to join the claims back to their
-- reservation.
--
-- now() (transaction start, the earliest reading in this transaction) for
-- the same reason SumUnexpiredReservationHolds uses it: a row on the
-- boundary is treated as still held.
SELECT r.id, r.uid, r.reserved_amount
FROM reservations r
WHERE r.account_holder = $1
  AND r.currency_id = $2
  AND r.expires_at > now()
ORDER BY r.id;

-- name: ListReservationOperationReceiptsForReservations :many
-- Every operation receipt belonging to the given reservations, with the
-- signature material a verifier needs and the reservation uid the digest
-- covers. Read by the gate OUTSIDE the transaction (an AuthVerifier may be a
-- remote call and financial.md forbids that inside one), so the answer is
-- authorized-but-not-current; the under-lock recompute is what makes that
-- safe (see postgres.ReserverStore.reserveWithQueries and I-65).
SELECT o.id, o.reservation_id, r.uid AS reservation_uid, o.operation, o.idempotency_key,
       o.amount, o.created_at, o.auth_digest, o.auth_signature, o.auth_key_id
FROM reservation_operation_receipts o
JOIN reservations r ON r.id = o.reservation_id
WHERE o.reservation_id = ANY(sqlc.arg(reservation_ids)::bigint[])
ORDER BY o.id;

-- name: ListReservationSettlementLegsForReservations :many
-- The settlement-leg half of the same fetch. Legs have no `operation`
-- column: the record kind IS the operation, and the caller labels these
-- core.ReservationOpSettlePartial when it rebuilds the digest preimage (see
-- that constant's doc comment for why the two record kinds must not share
-- one).
SELECT l.id, l.reservation_id, r.uid AS reservation_uid, l.idempotency_key,
       l.amount, l.created_at, l.auth_digest, l.auth_signature, l.auth_key_id
FROM reservation_settlement_legs l
JOIN reservations r ON r.id = l.reservation_id
WHERE l.reservation_id = ANY(sqlc.arg(reservation_ids)::bigint[])
ORDER BY l.id;
