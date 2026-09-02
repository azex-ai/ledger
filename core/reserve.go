package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

type ReservationStatus string

const (
	ReservationStatusActive   ReservationStatus = "active"
	ReservationStatusSettling ReservationStatus = "settling"
	ReservationStatusSettled  ReservationStatus = "settled"
	ReservationStatusReleased ReservationStatus = "released"
)

var reservationTransitions = map[ReservationStatus][]ReservationStatus{
	ReservationStatusActive:   {ReservationStatusSettling, ReservationStatusSettled, ReservationStatusReleased},
	ReservationStatusSettling: {ReservationStatusSettled, ReservationStatusReleased},
}

func (s ReservationStatus) IsValid() bool {
	switch s {
	case ReservationStatusActive, ReservationStatusSettling, ReservationStatusSettled, ReservationStatusReleased:
		return true
	}
	return false
}

func (s ReservationStatus) CanTransitionTo(target ReservationStatus) bool {
	for _, allowed := range reservationTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Reservation struct {
	UID            string            `json:"uid"`
	AccountHolder  int64             `json:"account_holder"`
	CurrencyUID    string            `json:"currency_uid"`
	ReservedAmount decimal.Decimal   `json:"reserved_amount"`
	SettledAmount  *decimal.Decimal  `json:"settled_amount,omitempty"`
	Status         ReservationStatus `json:"status"`
	JournalUID     string            `json:"journal_uid,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ReserveInput struct {
	AccountHolder  int64           `json:"account_holder"`
	CurrencyUID    string          `json:"currency_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	ExpiresIn      time.Duration   `json:"expires_in"`
	// RequireVerifiedBalance, when true, changes Reserve in two ways
	// (contracts §W2-1/§W2-2/§W2-3, docs/INVARIANTS.md I-32 and I-49):
	//
	//   - It refuses (wrapping ErrUnauthorizedJournal) unless every
	//     balance_role=available classification this holder has touched in
	//     CurrencyUID passes VerifiedBalanceReader's authorization check.
	//
	//   - It sizes the reservation off those checks' entries-only recomputes
	//     rather than off balance_checkpoints. So this is also a stricter
	//     AMOUNT check: an inflated checkpoint row cannot raise what a gated
	//     Reserve will lock, even when every journal is genuinely signed.
	//     Insufficiency under the recomputed base is ErrInsufficientBalance,
	//     not ErrUnauthorizedJournal.
	//
	// Because the gate may call a (possibly remote) AuthVerifier, it runs
	// before any transaction is opened -- setting this field on a Reserve
	// issued from inside a RunInTx callback is refused with ErrInvalidInput
	// rather than silently downgraded.
	//
	// Off by default: this library does not pick a threshold or a default
	// policy for when the extra check is warranted -- that decision, and
	// which calls it applies to, belongs entirely to the caller, made once
	// per Reserve call (contracts §W2-3: "机制在库，策略在消费方"). A
	// consumer that never sets this field sees no behavior change at all.
	RequireVerifiedBalance bool `json:"require_verified_balance,omitempty"`
}

func (i ReserveInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: reserve: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyUID == "" {
		return fmt.Errorf("core: reserve: currency_uid required: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: reserve: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: reserve: idempotency key required: %w", ErrInvalidInput)
	}
	if i.ExpiresIn < 0 {
		return fmt.Errorf("core: reserve: expires_in must not be negative: %w", ErrInvalidInput)
	}
	return nil
}

// SettleInput is the input for a one-shot settlement of an active reservation.
//
// IdempotencyKey is REQUIRED (I-3): the reservation's own state machine
// cannot serve as a replay signal here, because "settled" is terminal --
// replaying a lost-response retry of the exact same Settle call used to land
// on ErrInvalidTransition, indistinguishable from a genuine conflict (someone
// else settling a different amount, or the reservation having been released
// out from under the caller). A replayed key with the same amount now
// returns nil without re-applying; a replayed key with a different amount is
// ErrConflict; the same key reused against a different reservation is also
// ErrConflict.
//
// COMPOSITION WARNING. Settle does not move money -- the charge is a separate
// journal the caller posts, and the documented pattern (examples/billing) runs
// both inside one RunInTx. Retrying that whole block is only safe if the
// journal's idempotency key is reused too, exactly as this one must be
// (api-contract.md §9: the key is generated once by the initiator and reused
// across retries, never regenerated inside the retry path). Note what changed
// here: before this field existed, a retry of the block died at Settle with
// ErrInvalidTransition and took the transaction down with it, so a
// freshly-keyed charge could never land. That accident was doing real work.
// Now Settle correctly short-circuits to success, and the charge's own key is
// the only thing left standing between a retry and a double charge. Generate
// both keys outside the retry, not inside it.
//
// A second composition trap: a hold binds only OTHER RESERVATIONS (I-11). A
// direct journal on the same dimension — lock_funds, transfer_out, any
// template — can spend the reserved funds out from under an active
// reservation, and account_policies' min_balance check does not net out
// holds either. If that happens, the settlement journal posted alongside
// this Settle either drives the balance negative (no min_balance policy) or
// is rejected with ErrInsufficientBalance, rolling back the whole RunInTx
// and wedging the reservation until it expires. Consumers that need reserved
// funds to be unspendable must route all consumption through Reserve→Settle.
type SettleInput struct {
	ReservationUID string          `json:"reservation_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (i SettleInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: settle: reservation_uid required: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: settle: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: settle: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// ReleaseInput is the input to cancel an active (or settling) reservation.
//
// IdempotencyKey is REQUIRED (I-3) for the same reason SettleInput's is:
// "released" is terminal, so a lost-response retry of Release used to land
// on ErrInvalidTransition with no way to tell "I already did this" apart
// from a genuine conflict. A replayed key against the same reservation
// returns nil without re-applying; the same key reused against a different
// reservation is ErrConflict.
type ReleaseInput struct {
	ReservationUID string `json:"reservation_uid"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (i ReleaseInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: release: reservation_uid required: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: release: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// FinalizeSettlementInput is the input to complete a reservation that has
// been partially settled via SettlePartial.
//
// IdempotencyKey is REQUIRED (I-3) for the same reason SettleInput's and
// ReleaseInput's are: the settling -> settled transition is terminal, so a
// lost-response retry used to land on ErrInvalidTransition. A replayed key
// against the same reservation returns nil without re-applying; the same key
// reused against a different reservation is ErrConflict.
type FinalizeSettlementInput struct {
	ReservationUID string `json:"reservation_uid"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (i FinalizeSettlementInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: finalize settlement: reservation_uid required: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: finalize settlement: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// SettlePartialInput is the input for one increment of a partial settlement.
//
// IdempotencyKey is REQUIRED (I-3): SettlePartial is an accumulator
// (settled_amount += Amount), so without a durable dedup record a client
// retry of a lost response would double-apply the amount. A replayed key
// with the same amount succeeds without re-applying; a replayed key with a
// different amount is ErrConflict.
type SettlePartialInput struct {
	ReservationUID string          `json:"reservation_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (i SettlePartialInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: settle partial: reservation_uid required: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: settle partial: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: settle partial: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}
