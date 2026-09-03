// Package core: reservation_auth.go
//
// Canonical, deterministic byte encoding for a RESERVATION DISCHARGE CLAIM
// -- the record the application writes when a reservation stops holding
// money (a settlement, a partial settlement leg, a release, a finalize).
// Same shape and same rules as auth.go's per-journal digest: pure
// functions, no DB access, no signing call, so they are safe to run inside
// or outside a transaction; it is the caller's job
// (postgres.ReserverStore) to make sure the Attestor.Sign call this digest
// feeds into never happens while a transaction is open (financial.md).
//
// #### Why a discharge claim needs a signature at all ####
//
// docs/INVARIANTS.md I-49 established that the hold a gated Reserve
// subtracts may not be read from anything the application's own database
// credential can write. reservations.status and
// reservations.settled_amount are UPDATE-able by ledger_app (the guard
// permits exactly the legitimate transitions), and
// reservation_operation_receipts / reservation_settlement_legs are
// append-only but still INSERT-able by ledger_app -- the application is
// what writes them. In this threat model the application's credential IS
// the attacker, so any discharge the application can perform the attacker
// can perform, and no trigger, ACL or SECURITY DEFINER function can tell
// the two apart: same role, same statement.
//
// I-49's answer was to credit no discharge at all and trust only
// expires_at (a claim no credential can manufacture). That is safe and
// costs a settled or released reservation its full hold until expiry.
//
// This file implements the other answer, the one I-49 recorded as "not
// closed": make the discharge claim UNFORGEABLE instead of unnecessary.
// A signature is the only other signal that escapes the threat model,
// because the signing key is held by an Attestor the database credential
// does not have (remediation contract §7.18, lead's ruling under Aaron's
// 2026-09-03 mandate; tamper-evident design §0).
package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Reservation discharge operations. These are the values that go into
// reservation_operation_receipts.operation verbatim, plus one that does
// not exist as a column value at all:
//
//   - ReservationOpSettlePartial labels a reservation_settlement_legs row.
//     That table has no `operation` column -- the record kind IS the
//     operation -- so this constant exists to keep the two record kinds
//     from sharing a digest preimage. Without it, a signed 'settle' receipt
//     for amount X and a signed leg for the same amount X on the same
//     reservation would hash identically, and a signature minted for one
//     would verify as the other.
//
// Defined here rather than in the postgres adapter because they are part of
// the digest preimage, which is a wire contract (see
// CanonicalReservationDischargeDigest); the adapter's own private constants
// alias these so there is exactly one spelling of each.
const (
	ReservationOpSettle             = "settle"
	ReservationOpRelease            = "release"
	ReservationOpFinalizeSettlement = "finalize_settlement"
	ReservationOpSettlePartial      = "settle_partial"
)

// reservationDischargeDigestDomain domain-separates
// CanonicalReservationDischargeDigest's output. See authDigestDomain's doc
// comment in auth.go for the package-wide allocation table -- 0x11 is
// allocated there, and a breaking encoding change here MUST take a new
// byte rather than reusing this one.
const reservationDischargeDigestDomain = byte(0x11)

// ReservationDischargeIntent is the uid-space content of one discharge
// claim: which reservation stops holding money, by which operation, for how
// much, under which idempotency key, and at which instant the claim was
// recorded. Every field is covered by the digest; there is no
// deliberately-excluded field (contrast JournalInput's EventUID and
// Metadata, which CanonicalJournalDigest omits).
//
// ReservationUID, not an internal reservations.id: the digest is uid-space
// like every other cross-boundary contract in this library, so a verifier
// recomputing it never needs to agree with the writer about BIGSERIAL
// values.
//
// IdempotencyKey is what binds a signature to exactly one row. It carries a
// UNIQUE index on both discharge tables, and both tables refuse UPDATE and
// DELETE (migration 006), so a signature minted for one claim cannot be
// re-presented for another: replaying the row verbatim is rejected by the
// unique index, and changing any covered field breaks the digest. This is
// why the digest needs no per-reservation sequence number -- a monotonic
// counter would add nothing the key does not already provide, and the
// counter's value would not be knowable at signing time anyway (the row's
// BIGSERIAL id is assigned inside the transaction the signature has to
// precede).
type ReservationDischargeIntent struct {
	ReservationUID string
	// Operation is one of the ReservationOp* constants above.
	Operation string
	// Amount is the amount the claim records: the settled amount for
	// ReservationOpSettle and ReservationOpSettlePartial, zero for
	// ReservationOpRelease and ReservationOpFinalizeSettlement (which take
	// no amount and discharge the whole remaining hold).
	Amount decimal.Decimal
	// IdempotencyKey is the caller-supplied key the claim row is keyed by.
	IdempotencyKey string
	// RecordedAt is the instant the claim row's created_at will carry. It is
	// covered by the digest, which is why the writer must persist exactly
	// this value instead of letting the column default to now(): a digest
	// signed over one instant and a row storing a different one can never
	// be re-verified. Canonicalized (UTC, floored to microseconds) before
	// encoding, for the reason canonicalTimestamp's doc comment gives.
	RecordedAt time.Time
}

// CanonicalReservationDischargeDigest computes the deterministic,
// domain-separated SHA-256 digest of one discharge claim.
//
// Byte layout (all integers big-endian; LP(s) = BE32(len(utf8(s))) ||
// utf8(s), the same length-prefix CanonicalJournalDigest uses):
//
//	SHA-256(
//	  0x11                                              -- domain separator
//	  LP(in.ReservationUID)
//	  LP(in.Operation)
//	  LP(in.IdempotencyKey)
//	  LP(canonicalTimestamp(in.RecordedAt).Format(RFC3339Nano))
//	  EncodeAmount(in.Amount)                           -- 16 bytes, auth.go
//	)
//
// Returns core.ErrInvalidInput (via EncodeAmount) if Amount cannot be
// encoded losslessly at scale 18.
//
// Golden vectors pinning this exact layout live in reservation_auth_test.go;
// diverging from them is a breaking encoding change requiring a new domain
// separator, not a silent fix.
func CanonicalReservationDischargeDigest(in ReservationDischargeIntent) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(reservationDischargeDigestDomain)

	writeLenPrefixed(&buf, in.ReservationUID)
	writeLenPrefixed(&buf, in.Operation)
	writeLenPrefixed(&buf, in.IdempotencyKey)
	writeLenPrefixed(&buf, canonicalTimestamp(in.RecordedAt).Format(time.RFC3339Nano))

	amtBytes, err := EncodeAmount(in.Amount)
	if err != nil {
		return nil, fmt.Errorf("core: canonical reservation discharge digest: %w", err)
	}
	buf.Write(amtBytes)

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

// VerifyReservationDischargeAuth recomputes in's canonical digest, checks it
// against storedDigest (what was actually signed when the claim was
// written), then asks verifier to check signature/keyID against the
// recomputed digest. Mirrors VerifyJournalAuth exactly.
//
// Returns a non-nil error if storedDigest is empty (the claim was never
// signed), does not match the recomputed digest (the persisted row does not
// match what was signed), signature/keyID are empty, or verifier rejects
// them.
//
// It deliberately wraps NO new sentinel error. Its only caller
// (postgres.ReserverStore's gated hold computation) treats every non-nil
// answer identically -- the claim is not trusted, so the reservation keeps
// holding its full amount, which is I-49's conservative rule -- and the
// error never crosses an API boundary, so a new core sentinel would have to
// be bound into pkg/bizcode and pkg/httpx's code tables for a condition no
// HTTP response can ever carry. The reason string is for the log line the
// store emits, not for a caller to branch on.
//
// A nil verifier is not special-cased into success: it is a programming
// error to call this without one, and returning an error is the fail-closed
// answer (an unverifiable claim is an untrusted claim).
func VerifyReservationDischargeAuth(ctx context.Context, verifier AuthVerifier, in ReservationDischargeIntent, storedDigest, signature []byte, keyID string) error {
	if verifier == nil {
		return fmt.Errorf("core: verify reservation discharge auth: no auth verifier configured")
	}
	if len(storedDigest) == 0 {
		return fmt.Errorf("core: verify reservation discharge auth: claim %q has no stored digest", in.IdempotencyKey)
	}
	recomputed, err := CanonicalReservationDischargeDigest(in)
	if err != nil {
		return fmt.Errorf("core: verify reservation discharge auth: recompute digest: %w", err)
	}
	if !bytes.Equal(recomputed, storedDigest) {
		return fmt.Errorf("core: verify reservation discharge auth: claim %q stored digest does not match recomputed digest", in.IdempotencyKey)
	}
	if len(signature) == 0 || keyID == "" {
		return fmt.Errorf("core: verify reservation discharge auth: claim %q has no signature", in.IdempotencyKey)
	}
	if err := verifier.Verify(ctx, recomputed, signature, keyID); err != nil {
		return fmt.Errorf("core: verify reservation discharge auth: claim %q signature invalid: %w", in.IdempotencyKey, err)
	}
	return nil
}
