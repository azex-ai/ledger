package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Booking is the unified record replacing Deposit/Withdrawal.
// Its lifecycle is governed by the classification's state machine.
type Booking struct {
	ID               int64           `json:"id"`
	ClassificationID int64           `json:"classification_id"`
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	Amount           decimal.Decimal `json:"amount"`
	SettledAmount    decimal.Decimal `json:"settled_amount"`
	Status           Status          `json:"status"`
	ChannelName      string          `json:"channel_name"`
	ChannelRef       string          `json:"channel_ref"`
	// ReservationID and JournalID are nullable: NULL means "no reservation /
	// journal linked yet". Sentinel 0 cannot be used because Postgres needs a
	// real NULL to skip FK enforcement.
	ReservationID    *int64          `json:"reservation_id,omitempty"`
	JournalID        *int64          `json:"journal_id,omitempty"`
	IdempotencyKey   string          `json:"idempotency_key"`
	Metadata         map[string]any  `json:"metadata"`
	ExpiresAt        time.Time       `json:"expires_at"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// CreateBookingInput is the input to create a new booking.
type CreateBookingInput struct {
	ClassificationCode string          `json:"classification_code"`
	AccountHolder      int64           `json:"account_holder"`
	CurrencyID         int64           `json:"currency_id"`
	Amount             decimal.Decimal `json:"amount"`
	IdempotencyKey     string          `json:"idempotency_key"`
	ChannelName        string          `json:"channel_name"`
	Metadata           map[string]any  `json:"metadata"`
	ExpiresAt          time.Time       `json:"expires_at"`
}

func (i CreateBookingInput) Validate() error {
	if i.ClassificationCode == "" {
		return fmt.Errorf("core: booking: classification_code required: %w", ErrInvalidInput)
	}
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: booking: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyID <= 0 {
		return fmt.Errorf("core: booking: currency_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: booking: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: booking: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// TransitionInput is the input to advance a booking's state.
type TransitionInput struct {
	BookingID  int64           `json:"booking_id"`
	ToStatus   Status          `json:"to_status"`
	ChannelRef string          `json:"channel_ref"`
	Amount     decimal.Decimal `json:"amount"`
	Metadata   map[string]any  `json:"metadata"`
	ActorID    int64           `json:"actor_id"`
	// Source identifies the calling service or scope (e.g. "api", "worker", "webhook").
	Source     string          `json:"source"`
}

func (i TransitionInput) Validate() error {
	if i.BookingID <= 0 {
		return fmt.Errorf("core: booking: booking_id must be positive: %w", ErrInvalidInput)
	}
	if i.ToStatus == "" {
		return fmt.Errorf("core: booking: to_status required: %w", ErrInvalidInput)
	}
	if i.Amount.IsNegative() {
		return fmt.Errorf("core: booking: amount must not be negative: %w", ErrInvalidInput)
	}
	return nil
}

// BookingFilter is the filter for listing bookings.
type BookingFilter struct {
	AccountHolder    int64  `json:"account_holder"`
	ClassificationID int64  `json:"classification_id"`
	Status           string `json:"status"`
	Cursor           int64  `json:"cursor"`
	Limit            int    `json:"limit"`
}
