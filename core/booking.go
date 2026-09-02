package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Booking is the unified record replacing Deposit/Withdrawal.
// Its lifecycle is governed by the classification's state machine.
type Booking struct {
	UID               string          `json:"uid"`
	ClassificationUID string          `json:"classification_uid"`
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	Amount            decimal.Decimal `json:"amount"`
	SettledAmount     decimal.Decimal `json:"settled_amount"`
	Status            Status          `json:"status"`
	ChannelName       string          `json:"channel_name"`
	ChannelRef        string          `json:"channel_ref"`
	// ReservationUID and JournalUID are empty when "no reservation /
	// journal linked yet". Sentinel 0 cannot be used because Postgres needs a
	// real NULL to skip FK enforcement.
	ReservationUID string            `json:"reservation_uid,omitempty"`
	JournalUID     string            `json:"journal_uid,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// CreateBookingInput is the input to create a new booking.
type CreateBookingInput struct {
	ClassificationCode string            `json:"classification_code"`
	AccountHolder      int64             `json:"account_holder"`
	CurrencyUID        string            `json:"currency_uid"`
	Amount             decimal.Decimal   `json:"amount"`
	IdempotencyKey     string            `json:"idempotency_key"`
	ChannelName        string            `json:"channel_name"`
	Metadata           map[string]string `json:"metadata"`
	ExpiresAt          time.Time         `json:"expires_at"`
}

func (i CreateBookingInput) Validate() error {
	if i.ClassificationCode == "" {
		return fmt.Errorf("core: booking: classification_code required: %w", ErrInvalidInput)
	}
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: booking: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyUID == "" {
		return fmt.Errorf("core: booking: currency_uid required: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: booking: amount must be positive: %w", ErrInvalidInput)
	}
	if err := validateFreeformFields("booking", "", i.Metadata); err != nil {
		return err
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: booking: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// TransitionInput is the input to advance a booking's state.
type TransitionInput struct {
	BookingUID string `json:"booking_uid"`
	ToStatus   Status `json:"to_status"`
	ChannelRef string `json:"channel_ref"`
	// Amount is intentionally allowed to be zero: not every lifecycle
	// transition moves money (e.g. a pure status change like
	// "reviewing" -> "processing"). Amount is only required to be positive
	// where a transition actually triggers accounting — that is enforced by
	// JournalInput.Validate at the point a journal is composed for the
	// transition, not here.
	Amount   decimal.Decimal   `json:"amount"`
	Metadata map[string]string `json:"metadata"`
	ActorID  int64             `json:"actor_id"`
	// Source identifies the calling service or scope (e.g. "api", "worker", "webhook").
	Source string `json:"source"`
	// IdempotencyKey is REQUIRED, like every other write in this package
	// (I-3: every state-changing operation requires a key). It is checked
	// under the booking's row lock, before the lifecycle transition gate,
	// against a durable booking_transition_receipts row (booking, to_status,
	// channel_ref, amount, and the event_id the original call produced): a
	// replay with the same key and the same payload always returns the
	// original event, even after the booking has since moved on to a later
	// status; the same key with a different payload is ErrConflict.
	//
	// Deriving the key is the caller's responsibility, and it is not simply
	// "booking uid + to_status": a booking's lifecycle can legitimately
	// revisit the same status more than once (e.g. the withdrawal preset's
	// failed -> reserved retry edge, or the sweep preset's failed -> pending
	// revival edge). A key that ignores which *occurrence* of that status a
	// call is describing collides across two genuinely different, legitimate
	// transitions -- the second one would silently short-circuit to the
	// first one's receipt instead of applying. The key must be deterministic
	// across retries of the exact same source event (system-driven callers:
	// derive from that event's own identity -- an upstream tx hash, event
	// id, or attempt count) or a client-generated random UUID carried on the
	// caller's Idempotency-Key header (api-contract.md §9). See
	// service/onchain.go's Transition call sites for worked examples.
	//
	// Separately from this key, Transition also has a narrower,
	// state-comparison-based idempotency path for calls that race to apply
	// the *same* occurrence concurrently under two different (but both
	// freshly-derived) keys: when the booking is already AT ToStatus with no
	// matching receipt for this call's key, it compares the latest event's
	// payload against this call and either returns that event (matching
	// payload) or ErrConflict (diverging payload) -- see
	// idempotentTransitionEvent in postgres/booking_store.go. That path only
	// fires when nothing has moved the booking away from ToStatus since; a
	// later, distinct occurrence targeting the same status necessarily
	// passes through an intervening transition first, so it never mis-fires
	// on the retry-to-the-same-status case above.
	IdempotencyKey string `json:"idempotency_key"`
}

func (i TransitionInput) Validate() error {
	if i.BookingUID == "" {
		return fmt.Errorf("core: booking: booking_uid required: %w", ErrInvalidInput)
	}
	if err := validateFreeformFields("transition", i.Source, i.Metadata); err != nil {
		return err
	}
	if i.ToStatus == "" {
		return fmt.Errorf("core: booking: to_status required: %w", ErrInvalidInput)
	}
	// Zero is deliberately allowed here (see TransitionInput.Amount doc) —
	// only negative amounts are a shape error at this layer.
	if i.Amount.IsNegative() {
		return fmt.Errorf("core: booking: amount must not be negative: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: booking: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// BookingFilter is the filter for listing bookings.
type BookingFilter struct {
	AccountHolder     int64  `json:"account_holder"`
	ClassificationUID string `json:"classification_uid"`
	Status            string `json:"status"`
	// Cursor is the opaque keyset cursor from the previous page ("" = start).
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}
