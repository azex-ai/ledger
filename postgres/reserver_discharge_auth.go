package postgres

// Signed reservation discharge claims: the write half (attestDischarge) and
// the read half (verifiedDischarges) of docs/INVARIANTS.md I-65.
//
// I-49 fixed the hold a gated Reserve subtracts by refusing to credit any
// discharge claim at all, because every such claim -- reservations.status,
// reservations.settled_amount, an appended settlement leg, an appended
// operation receipt -- is writable or appendable with the application's own
// database credential, and in this threat model that credential IS the
// attacker. The cost, stated and accepted there, is that a settled or
// released reservation keeps holding its full reserved_amount until
// expires_at.
//
// I-49 also named the only two signals that escape the threat model: the
// passage of time (which it used) and a signature over a key the database
// credential does not hold (which it recorded as "not closed"). This file
// closes it, per the remediation contract §7.18 ruling.
//
// The two halves are deliberately on opposite sides of the transaction
// boundary, for the same reason V and E are in Reserve:
//
//   - attestDischarge runs BEFORE the write transaction opens (pool mode
//     only). An Attestor is permitted to be a remote call, and financial.md
//     forbids external calls inside a transaction.
//   - verifiedDischarges runs BEFORE the reserve transaction opens, next to
//     requireVerifiedAvailableBalance's V, for the identical reason applied
//     to AuthVerifier.
//
// Both therefore produce answers that are authorized but not current, and
// neither is used on its own: reserveWithQueries re-reads the reservations
// themselves under the (holder, currency) advisory lock, in pure SQL, and
// only subtracts a pre-verified discharge from a reservation the lock-side
// read still shows as unexpired. A reservation or a claim that lands in the
// window is simply not credited, which is the conservative direction.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// dischargeCreatedAtDefault is the value the two discharge INSERTs treat as
// "no signed instant -- let the column default decide". It must be exactly
// the `'epoch'::timestamptz` those queries compare against (see
// InsertReservationOperationReceipt in reservations.sql); a Go zero
// time.Time is year 1, not the epoch, so it would land in the row instead
// of being replaced by now().
var dischargeCreatedAtDefault = time.Unix(0, 0).UTC()

// reservationDischargeAuth is what attestDischarge produces: the instant to
// persist as the claim row's created_at, plus the signature material. The
// zero value is the unsigned claim -- empty digest/signature/keyID and the
// epoch sentinel for created_at -- which is what every path without an
// Attestor writes, and what a gated Reserve refuses to credit.
type reservationDischargeAuth struct {
	createdAt time.Time
	digest    []byte
	signature []byte
	keyID     string
}

// unsignedDischarge is the unsigned claim spelled out, so a call site reads
// as a decision rather than as an omission.
//
// digest/signature are EMPTY-BUT-NON-NIL slices, not nil: the columns are
// NOT NULL DEFAULT ''::bytea (migration 028, per this schema's no-NULL
// rule), and pgx encodes a nil []byte as SQL NULL -- which is a 23502, not
// an empty value. Measured: every unsigned Release/Settle failed with
// "invalid database input" until these were non-nil.
func unsignedDischarge() reservationDischargeAuth {
	return reservationDischargeAuth{
		createdAt: dischargeCreatedAtDefault,
		digest:    []byte{},
		signature: []byte{},
	}
}

// attestDischarge signs the discharge claim about to be written, outside any
// transaction. Called from the top of Settle / SettlePartial / Release /
// FinalizeSettlement, strictly before pool.Begin.
//
// Returns an unsigned claim, with no error, in three cases -- all of which
// leave a gated Reserve holding the reservation in full until expiry, which
// is I-49's rule and is safe:
//
//   - no Attestor configured (WithAuth never called): signing is off for
//     this deployment.
//   - tx mode (s.pool == nil, i.e. a store bound via WithDB from inside a
//     caller's RunInTx): the caller's transaction is already open, so there
//     is no safe point left to call a possibly-remote Attestor. Fails closed
//     by not signing rather than by dialling out, exactly as PostJournal's
//     tx-mode branch does. A consumer who needs a signed discharge must call
//     these four operations on the top-level store, before or after their own
//     transaction, not inside it.
//   - the idempotency key already has a claim row: this is a replay, the
//     write path will short-circuit on it and insert nothing, so signing
//     would burn a signer round trip for a row that is never written. Same
//     pre-check attestJournal makes for the same reason.
//
// An Attestor that errors is NOT downgraded to unsigned: a signer that is
// momentarily unreachable is reported (wrapping core.ErrAttestorUnavailable,
// which core.IsRetryable classifies as retryable) so the caller retries with
// the same key, rather than silently writing a claim that will never
// discharge its hold. Same choice PostJournal makes.
func (s *ReserverStore) attestDischarge(ctx context.Context, q *sqlcgen.Queries, intent core.ReservationDischargeIntent) (reservationDischargeAuth, error) {
	if s.attestor == nil || s.pool == nil {
		return unsignedDischarge(), nil
	}

	replayed, err := s.dischargeClaimExists(ctx, q, intent.Operation, intent.IdempotencyKey)
	if err != nil {
		return reservationDischargeAuth{}, err
	}
	if replayed {
		return unsignedDischarge(), nil
	}

	intent.RecordedAt = time.Now()
	digest, err := core.CanonicalReservationDischargeDigest(intent)
	if err != nil {
		return reservationDischargeAuth{}, fmt.Errorf("postgres: %s: canonical discharge digest: %w", intent.Operation, err)
	}
	signature, keyID, err := s.attestor.Sign(ctx, digest)
	if err != nil {
		return reservationDischargeAuth{}, fmt.Errorf("postgres: %s: attestor sign: %w: %w", intent.Operation, err, core.ErrAttestorUnavailable)
	}
	return reservationDischargeAuth{
		createdAt: intent.RecordedAt,
		digest:    digest,
		signature: signature,
		keyID:     keyID,
	}, nil
}

// dischargeClaimExists reports whether operation's idempotency key already
// has a claim row, looking in the table that operation writes.
func (s *ReserverStore) dischargeClaimExists(ctx context.Context, q *sqlcgen.Queries, operation, idempotencyKey string) (bool, error) {
	var err error
	if operation == core.ReservationOpSettlePartial {
		_, err = q.GetSettlementLegByIdempotencyKey(ctx, idempotencyKey)
	} else {
		_, err = q.GetReservationOperationReceiptByIdempotencyKey(ctx, idempotencyKey)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("postgres: %s: check idempotency before signing: %w", operation, err)
	}
}

// verifiedDischarges computes, for every not-yet-expired reservation the
// holder has in the currency, how much of its reserved_amount has been
// discharged by a claim whose signature VERIFIES. Keyed by reservations.id
// so reserveWithQueries can look each one up against the rows it re-reads
// under the advisory lock.
//
// Returns (nil, nil) when the signed path is not engaged (no Attestor or no
// AuthVerifier configured). reserveWithQueries reads that as "use
// SumUnexpiredReservationHolds", which is I-49's rule unchanged -- not as
// "nothing is discharged", which would compute the same number by a
// different query and make the no-attestor path silently depend on this
// one being correct.
//
// That early return is defence in depth, not a behavioural switch, and it
// was measured rather than assumed (reversal experiment, 2026-09-03):
// deleting it leaves every pin green, because a nil verifier makes
// core.VerifyReservationDischargeAuth fail for every claim, which distrusts
// every reservation, which holds every reserved_amount in full -- the same
// number SumUnexpiredReservationHolds returns. It stays because "the
// unconfigured deployment runs the code path it ran before 028" is a
// stronger guarantee than "the new code path happens to agree", and the
// second one is what a future edit can break silently.
//
// The per-reservation rule, and why each branch is the conservative one:
//
//   - Any claim on the reservation that fails verification, or carries no
//     signature at all, disqualifies the WHOLE reservation: its discharge is
//     zero and it holds its full reserved_amount. Crediting the claims that
//     did verify while ignoring one that did not would let an attacker
//     append a forged claim next to genuine ones and still collect the
//     genuine discharge -- and, worse, would make a tampered reservation
//     indistinguishable from a clean one. Same fail-closed shape as I-32's
//     "one unauthorized classification refuses the whole reservation".
//   - A verified 'release', 'settle' or 'finalize_settlement' claim
//     discharges the reservation ENTIRELY. All three are terminal: Release
//     returns the whole amount, Settle records the settled portion and
//     implicitly releases the remainder, FinalizeSettlement does the same
//     for the accumulated legs. The settled portion has already left through
//     its own journal, so E has already fallen by it -- which is precisely
//     the double-count I-49 had to accept and this removes.
//   - Otherwise the discharge is the sum of the verified settlement legs, so
//     a partially-settled reservation holds only its unsettled remainder.
//   - The result is clamped to reserved_amount, so no arithmetic on claim
//     amounts can turn a hold negative and subsidise another reservation.
//     A legitimate claim can never exceed it (chk_settled_lte_reserved), so
//     this only ever fires on data that should already have failed
//     verification.
//
// Placement: this runs OUTSIDE the transaction because AuthVerifier may be
// remote. Its answer can therefore be stale in one direction only -- a claim
// written after it ran is not credited -- because a claim row can never be
// removed or altered (migration 006 refuses UPDATE and DELETE on both
// tables), so a discharge this function verified cannot stop being true.
// Staleness that under-credits a discharge holds more money, never less.
func (s *ReserverStore) verifiedDischarges(ctx context.Context, holder, currencyID int64) (map[int64]decimal.Decimal, error) {
	if s.attestor == nil || s.verifier == nil {
		return nil, nil
	}

	reservations, err := s.q.ListUnexpiredReservationHolds(ctx, sqlcgen.ListUnexpiredReservationHoldsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: verified discharges: list unexpired reservations: %w", err)
	}
	if len(reservations) == 0 {
		return map[int64]decimal.Decimal{}, nil
	}

	ids := make([]int64, 0, len(reservations))
	reserved := make(map[int64]decimal.Decimal, len(reservations))
	for _, r := range reservations {
		amount, err := numericToDecimal(r.ReservedAmount)
		if err != nil {
			return nil, fmt.Errorf("postgres: reserve: verified discharges: convert reserved amount of reservation %d: %w", r.ID, err)
		}
		ids = append(ids, r.ID)
		reserved[r.ID] = amount
	}

	receipts, err := s.q.ListReservationOperationReceiptsForReservations(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: verified discharges: list operation receipts: %w", err)
	}
	legs, err := s.q.ListReservationSettlementLegsForReservations(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: verified discharges: list settlement legs: %w", err)
	}

	// terminal[id] means a verified terminal claim discharged the whole
	// reservation; distrusted[id] means at least one claim on it did not
	// verify, which zeroes its discharge regardless of what else did.
	terminal := make(map[int64]bool, len(ids))
	distrusted := make(map[int64]bool, len(ids))
	partial := make(map[int64]decimal.Decimal, len(ids))

	for _, r := range receipts {
		amount, err := numericToDecimal(r.Amount)
		if err != nil {
			return nil, fmt.Errorf("postgres: reserve: verified discharges: convert receipt %d amount: %w", r.ID, err)
		}
		intent := core.ReservationDischargeIntent{
			ReservationUID: pgToUID(r.ReservationUid),
			Operation:      r.Operation,
			Amount:         amount,
			IdempotencyKey: r.IdempotencyKey,
			RecordedAt:     r.CreatedAt,
		}
		if err := core.VerifyReservationDischargeAuth(ctx, s.verifier, intent, r.AuthDigest, r.AuthSignature, r.AuthKeyID); err != nil {
			s.reportUnverifiedDischarge(intent, err)
			distrusted[r.ReservationID] = true
			continue
		}
		terminal[r.ReservationID] = true
	}

	for _, l := range legs {
		amount, err := numericToDecimal(l.Amount)
		if err != nil {
			return nil, fmt.Errorf("postgres: reserve: verified discharges: convert leg %d amount: %w", l.ID, err)
		}
		intent := core.ReservationDischargeIntent{
			ReservationUID: pgToUID(l.ReservationUid),
			Operation:      core.ReservationOpSettlePartial,
			Amount:         amount,
			IdempotencyKey: l.IdempotencyKey,
			RecordedAt:     l.CreatedAt,
		}
		if err := core.VerifyReservationDischargeAuth(ctx, s.verifier, intent, l.AuthDigest, l.AuthSignature, l.AuthKeyID); err != nil {
			s.reportUnverifiedDischarge(intent, err)
			distrusted[l.ReservationID] = true
			continue
		}
		partial[l.ReservationID] = partial[l.ReservationID].Add(amount)
	}

	out := make(map[int64]decimal.Decimal, len(ids))
	for _, id := range ids {
		switch {
		case distrusted[id]:
			out[id] = decimal.Zero
		case terminal[id]:
			out[id] = reserved[id]
		default:
			out[id] = decimal.Min(partial[id], reserved[id])
		}
	}
	return out, nil
}

// reportUnverifiedDischarge makes the degradation visible. The gate's
// reaction to an unverifiable claim is to hold the funds, which is safe but
// silent, and a claim that fails verification on an append-only row is
// tamper evidence -- exactly the class working-agreements §3 says must never
// be indistinguishable from "nothing happened".
//
// Logged at Warn rather than returned as an error on purpose: the caller
// asked to reserve money, and the answer to "one of your discharge claims
// does not verify" is "you have less available than you thought"
// (core.ErrInsufficientBalance if it matters), not "your reserve call is
// broken". Turning it into a hard failure would also make a single bad row
// permanently un-reservable for that holder, which is a denial of service an
// attacker could trigger with one INSERT.
func (s *ReserverStore) reportUnverifiedDischarge(intent core.ReservationDischargeIntent, err error) {
	s.logger.Warn("ledger: reservation discharge claim does not verify; the reservation keeps holding its full amount",
		"reservation_uid", intent.ReservationUID,
		"operation", intent.Operation,
		"idempotency_key", intent.IdempotencyKey,
		"amount", intent.Amount.String(),
		"error", err.Error(),
	)
}

// sumHoldsIgnoringDischarge is I-49's hold: the full reserved_amount of
// every not-yet-expired reservation on the dimension, crediting nothing.
// Reached when the gate ran but signing is not configured, so no discharge
// claim can be trusted. Kept as a wrapper around the SQL SUM rather than
// folded into the per-row path below, so the no-Attestor deployment executes
// the identical query it did before 028 (working-agreements §1: the fallback
// must not start depending on the new code being right).
//
// Runs on qtx, i.e. inside the (holder, currency) advisory lock, in pure
// SQL: a reservation that commits in the gate's window has to be visible,
// which is the same over-sell race I-4/I-11 exist to close.
func (s *ReserverStore) sumHoldsIgnoringDischarge(ctx context.Context, qtx *sqlcgen.Queries, holder, currencyID int64) (decimal.Decimal, error) {
	raw, err := qtx.SumUnexpiredReservationHolds(ctx, sqlcgen.SumUnexpiredReservationHoldsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: reserve: sum outstanding holds: %w", err)
	}
	held, err := anyToDecimal(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: reserve: convert outstanding holds: %w", err)
	}
	return held, nil
}

// sumHoldsNetOfVerifiedDischarge is I-65's hold:
//
//	Σ over the holder's not-yet-expired reservations in the currency
//	  of max(0, reserved_amount − verified discharge for that reservation)
//
// The reservations are re-read HERE, under the (holder, currency) advisory
// lock, from columns ledger_reservations_guard refuses to let anyone change
// (reserved_amount, expires_at). Only the DISCHARGE comes from outside the
// transaction, because deciding it needs a possibly-remote AuthVerifier.
// That split is what makes the pair safe:
//
//   - A reservation created in the gate's window appears in this read but
//     not in verifiedDischarges, so it is credited nothing and holds in
//     full. Conservative.
//   - A reservation that expired in the window appears in verifiedDischarges
//     but not in this read, so it is simply not held. Correct: expiry is the
//     one discharge signal no credential can manufacture.
//   - A claim written in the window is not in verifiedDischarges either, so
//     it discharges nothing yet. Conservative, and self-correcting on the
//     next call — claim rows are append-only (migration 006), so a verified
//     discharge can never later become false.
//
// The floor at zero is per reservation, not on the total, so one
// reservation's arithmetic can never subsidise another's hold.
func (s *ReserverStore) sumHoldsNetOfVerifiedDischarge(ctx context.Context, qtx *sqlcgen.Queries, holder, currencyID int64, discharges map[int64]decimal.Decimal) (decimal.Decimal, error) {
	rows, err := qtx.ListUnexpiredReservationHolds(ctx, sqlcgen.ListUnexpiredReservationHoldsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: reserve: list outstanding holds: %w", err)
	}

	total := decimal.Zero
	for _, r := range rows {
		reserved, err := numericToDecimal(r.ReservedAmount)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: reserve: convert reserved amount of reservation %d: %w", r.ID, err)
		}
		outstanding := reserved.Sub(discharges[r.ID])
		if outstanding.IsNegative() {
			outstanding = decimal.Zero
		}
		total = total.Add(outstanding)
	}
	return total, nil
}
